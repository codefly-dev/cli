package control

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/engine"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

// serviceBehavior resolves a service and binds it to the plane's shared host.
func (p *planeImpl) serviceBehavior(ctx context.Context, name string) (*engine.Service, error) {
	if p.host == nil {
		return nil, fmt.Errorf("control plane has no workspace host")
	}
	_, _, service, err := p.loadTarget(ctx, name)
	if err != nil {
		return nil, err
	}
	return p.host.Service(engine.ServiceTarget{Name: service.Name, Root: service.Dir()})
}

// ListCommands returns the plugin-owned commands a service exposes.
func (p *planeImpl) ListCommands(ctx context.Context, service string) ([]Command, error) {
	behavior, err := p.serviceBehavior(ctx, service)
	if err != nil {
		return nil, err
	}
	resp, err := behavior.ListCommands(ctx, &agentv0.ListCommandsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list commands: %w", err)
	}
	commands := make([]Command, 0, len(resp.Commands))
	for _, cmd := range resp.Commands {
		commands = append(commands, Command{
			Name:        cmd.Name,
			Description: cmd.Description,
			Usage:       cmd.Usage,
			Tags:        cmd.Tags,
			Destructive: cmd.Destructive,
		})
	}
	return commands, nil
}

// RunCommand invokes a plugin-owned command. A plugin-reported failure is
// returned as a non-zero exit code (with the plugin's error text), not a Go
// error — the call itself succeeded.
func (p *planeImpl) RunCommand(ctx context.Context, req RunCommandRequest) (CommandResult, error) {
	if req.Command == "" {
		return CommandResult{}, fmt.Errorf("command is required")
	}
	behavior, err := p.serviceBehavior(ctx, req.Service)
	if err != nil {
		return CommandResult{}, err
	}
	resp, err := behavior.RunCommand(ctx, &agentv0.RunPluginCommandRequest{
		Command: req.Command,
		Args:    req.Args,
	})
	if err != nil {
		return CommandResult{}, fmt.Errorf("run command %q: %w", req.Command, err)
	}
	if !resp.Success {
		return CommandResult{ExitCode: 1, Output: resp.Error}, nil
	}
	return CommandResult{ExitCode: 0, Output: resp.Output}, nil
}
