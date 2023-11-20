package golang

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/codefly-dev/cli/pkg/monitoring"
	"github.com/codefly-dev/cli/pkg/plugins"
	"github.com/codefly-dev/core/shared"
)

type Runner struct {
	Name  string
	Dir   string
	Args  []string
	Envs  []string
	Debug bool

	ServiceLogger *plugins.ServiceLogger
	PluginLogger  *plugins.PluginLogger

	Cmd   *exec.Cmd
	clean func()

	// internal
	killed bool
}

func (g *Runner) Init(ctx context.Context) error {
	g.killed = false
	var clean func()
	var err error
	if !g.Debug {
		clean, err = g.NormalCmd(ctx)
	} else {
		clean, err = g.debugCmd(ctx)
	}
	g.clean = clean
	if output, ok := shared.IsOutputError(err); ok {
		return shared.Wrapf(output, "cannot build cmd")
	}
	return nil
}

func (g *Runner) Run(ctx context.Context) (*monitoring.TrackedProcess, error) {
	// Setup variables once
	g.Cmd.Env = g.Envs

	err := shared.WrapStart(g.Cmd, g.ServiceLogger)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot wrap execution of cmd")
	}
	if g.killed {
		return &monitoring.TrackedProcess{PID: g.Cmd.Process.Pid, Killed: true}, nil
	}
	return &monitoring.TrackedProcess{PID: g.Cmd.Process.Pid}, nil
}

func (g *Runner) debugCmd(ctx context.Context) (func(), error) {
	g.PluginLogger.Info("running in DEBUG mode")
	// Build with debug options
	tmp := os.TempDir()
	target := fmt.Sprintf("%s/main", tmp)
	clean := func() {
		_ = os.Remove(target)
	}

	args := []string{"build", "-gcflags", "all=-N -l", "-o", target}
	args = append(args, g.Args...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = g.Dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		g.killed = true
		output := shared.NewOutputError(string(out))
		return nil, output
	}
	g.PluginLogger.Info("[go:runner] successfully built the debug binary")
	cmd = exec.CommandContext(ctx, target)
	cmd.Dir = g.Dir
	g.Cmd = cmd
	return clean, nil
}

func (g *Runner) NormalCmd(ctx context.Context) (func(), error) {
	g.PluginLogger.Info("[go::runner] building binary in normal mode")
	// Build with debug options
	tmp := os.TempDir()
	target := fmt.Sprintf("%s/main", tmp)
	clean := func() {
		_ = os.Remove(target)
	}

	args := []string{"build", "-o", target}
	args = append(args, g.Args...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = g.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.killed = true
		output := shared.NewOutputError(string(out))
		return nil, output
	}
	g.PluginLogger.Info("[go::runner] successfully built the regular binary")

	cmd = exec.CommandContext(ctx, target)
	cmd.Dir = g.Dir
	g.Cmd = cmd
	return clean, nil
}

func (g *Runner) Kill() error {
	if g.killed {
		return nil
	}
	g.killed = true
	if g.clean != nil {
		g.clean()
	}
	err := g.Cmd.Process.Kill()
	if err != nil {
		err = g.Cmd.Wait()
		if err != nil {
			return shared.Wrapf(err, "cannot wait for process to die")
		}
		return shared.Wrapf(err, "cannot kill process")
	}
	return nil
}
