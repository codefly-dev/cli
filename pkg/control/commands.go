package control

import (
	"context"
	"fmt"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/services"
)

// This file lifts the CommandRunner group. Listing and running plugin-owned
// commands IS special (per-service, plugin-defined) behavior, so it goes through
// the plugin agent over gRPC. It acquires the plugin the same way the CLI/MCP do
// — services.Load + LoadBuilder — not the Gateway's manager.Load path.

// loadInstance resolves a service and loads its plugin instance so the agent
// client is usable (builder loaded, plugin context established), mirroring the
// MCP getServiceAndInstance sequence.
func (p *planeImpl) loadInstance(ctx context.Context, name string) (*services.Instance, error) {
	ws, module, service, err := p.loadTarget(ctx, name)
	if err != nil {
		return nil, err
	}
	instance, err := services.Load(ctx, ws, module, service)
	if err != nil {
		return nil, fmt.Errorf("load service instance: %w", err)
	}
	if err := instance.LoadBuilder(ctx); err != nil {
		return nil, fmt.Errorf("load builder: %w", err)
	}
	if _, err := instance.Builder.Load(ctx); err != nil {
		return nil, fmt.Errorf("load plugin context: %w", err)
	}
	return instance, nil
}

// ListCommands returns the plugin-owned commands a service exposes.
func (p *planeImpl) ListCommands(ctx context.Context, service string) ([]Command, error) {
	instance, err := p.loadInstance(ctx, service)
	if err != nil {
		return nil, err
	}
	resp, err := instance.Agent.ListCommands(ctx, &agentv0.ListCommandsRequest{})
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
	instance, err := p.loadInstance(ctx, req.Service)
	if err != nil {
		return CommandResult{}, err
	}
	resp, err := instance.Agent.RunPluginCommand(ctx, &agentv0.RunPluginCommandRequest{
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
