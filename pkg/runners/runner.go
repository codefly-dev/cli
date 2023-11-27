package runners

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"

	"github.com/codefly-dev/cli/pkg/monitoring"
	"github.com/codefly-dev/core/agents"

	"github.com/codefly-dev/core/shared"
	"github.com/pkg/errors"
)

type Runner struct {
	Name  string
	Bin   string
	Dir   string
	Args  []string
	Envs  []string
	Debug bool

	ServiceLogger *agents.ServiceLogger
	AgentLogger   *agents.AgentLogger

	Wait bool

	Cmd *exec.Cmd

	// internal
	killed bool
}

func (g *Runner) Init(ctx context.Context) error {
	g.killed = false
	g.Cmd = exec.CommandContext(ctx, g.Bin, g.Args...)
	return nil
}

func (g *Runner) Run(ctx context.Context) (*monitoring.TrackedProcess, error) {
	// Setup variables once
	g.Cmd.Env = g.Envs
	g.Cmd.Dir = g.Dir
	if g.Wait {
		err := WrapStartDebug(g.Cmd, g.AgentLogger)
		if err != nil {
			return nil, g.AgentLogger.Wrapf(err, "cannot wrap execution of cmd")
		}
	} else {
		err := WrapStart(g.Cmd, g.ServiceLogger, g.AgentLogger)
		if err != nil {
			return nil, shared.Wrapf(err, "cannot wrap execution of cmd")
		}
	}
	if g.killed {
		return &monitoring.TrackedProcess{PID: g.Cmd.Process.Pid, Killed: true}, nil
	}
	return &monitoring.TrackedProcess{PID: g.Cmd.Process.Pid}, nil
}

func (g *Runner) Kill() error {
	if g.killed {
		return nil
	}
	g.killed = true
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

func WrapStart(cmd *exec.Cmd, loggers ...shared.BaseLogger) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "cannot create stdout pipe")
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return errors.Wrap(err, "cannot create stderr pipe")
	}

	var ws []io.Writer
	var errorWs []io.Writer
	for _, logger := range loggers {
		ws = append(ws, logger)
		errorWs = append(errorWs, logger)
	}
	go ForwardLogs(stdout, ws...)

	//	catch the error
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	errorWs = append(errorWs, w)

	go ForwardLogs(stderr, ws...)

	err = cmd.Start()
	if err != nil {
		for _, logger := range loggers {
			logger.Debugf("OOPS got error %v", err)
		}
		_ = w.Flush()
	}
	return nil
}

func WrapStartDebug(cmd *exec.Cmd, logger *agents.AgentLogger) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		logger.Debugf("OOPS got error %v %v", err, string(out))
	}
	return nil
}

func ForwardLogs(r io.ReadCloser, ws ...io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		t := scanner.Text()
		for _, w := range ws {
			_, _ = w.Write([]byte(t))
		}
	}

	if err := scanner.Err(); err != nil {
		for _, w := range ws {
			_, _ = w.Write([]byte(err.Error()))
		}
	}

	_ = r.Close()
}
