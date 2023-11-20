package python

import (
	"context"
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

func (g *Runner) Run(ctx context.Context) (*monitoring.TrackedProcess, error) {
	clean, err := g.NormalCmd(ctx)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot build cmd")
	}
	g.clean = clean
	if output, ok := shared.IsOutputError(err); ok {
		return nil, shared.Wrapf(output, "cannot build cmd")
	}

	// Setup variables once
	g.Cmd.Env = g.Envs

	err = shared.WrapStart(g.Cmd, g.ServiceLogger)
	if err != nil {
		return nil, shared.Wrapf(err, "cannot wrap execution of cmd")
	}
	if g.killed {
		return &monitoring.TrackedProcess{PID: g.Cmd.Process.Pid, Killed: true}, nil
	}
	return &monitoring.TrackedProcess{PID: g.Cmd.Process.Pid}, nil
}

func (g *Runner) NormalCmd(ctx context.Context) (func(), error) {
	g.PluginLogger.Info("running in NORMAL mode")
	cmd := exec.CommandContext(ctx, "python", g.Args...)
	cmd.Dir = g.Dir
	g.Cmd = cmd
	return nil, nil
}

func (g *Runner) Kill() error {
	if g.killed {
		return nil
	}
	g.killed = true
	g.clean()
	err := g.Cmd.Process.Kill()
	if err != nil {
		return shared.Wrapf(err, "cannot kill process")
	}
	err = g.Cmd.Wait()
	if err != nil {
		return shared.Wrapf(err, "cannot wait for process to die")
	}
	return nil
}
