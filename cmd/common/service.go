package common

import (
	"context"
	"fmt"

	resources "github.com/codefly-dev/core/resources"
)

// LoadRequiredE resolves the requested or active service without terminating
// the process. RunE commands should use this form so Cobra can propagate the
// failure and deferred cleanup still runs.
func LoadRequiredE(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, *resources.Service, error) {
	return loadRequiredE(ctx, args, LoadActiveContext)
}

// LoadRequiredNonInteractiveE is the headless service resolver. An explicit
// positional service always wins; otherwise the current path or a unique
// workspace service is used without ever consulting a terminal.
func LoadRequiredNonInteractiveE(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, *resources.Service, error) {
	return loadRequiredE(ctx, args, LoadActiveContextNonInteractive)
}

func loadRequiredE(ctx context.Context, args []string, loadActive func(context.Context) (*ActiveContext, error)) (*resources.Workspace, *resources.Module, *resources.Service, error) {
	if len(args) > 0 {
		// Service name provided — just load the workspace and look it up directly.
		// Don't trigger auto-resolve/picker.
		workspace, err := resources.FindWorkspaceUp(ctx)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot find workspace: %w", err)
		}
		if workspace == nil {
			return nil, nil, nil, fmt.Errorf("no workspace found")
		}
		service, module, err := workspace.FindUniqueModuleServiceByName(ctx, args[0])
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot find service %s: %w", args[0], err)
		}
		return workspace, module, service, nil
	}
	// No args — use active context (may prompt for selection).
	active, err := loadActive(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cannot load active context: %w", err)
	}
	if active.Workspace == nil || active.Module == nil || active.Service == nil {
		return nil, nil, nil, fmt.Errorf("active context does not contain a workspace, module, and service")
	}
	workspace, module, service := active.Workspace, active.Module, active.Service
	service.WithModule(module.Name)
	return workspace, module, service, nil
}

// LoadRequiredModuleE resolves an explicitly named or active module without
// terminating the process.
func LoadRequiredModuleE(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, error) {
	if len(args) > 0 {
		workspace, err := LoadWorkspace(ctx)
		if err != nil {
			return nil, nil, err
		}
		module, err := workspace.LoadModuleFromName(ctx, args[0])
		if err != nil {
			return nil, nil, fmt.Errorf("cannot load module %s: %w", args[0], err)
		}
		return workspace, module, nil
	}
	workspace, err := LoadWorkspace(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot load workspace: %w", err)
	}
	module, err := LoadModule(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot load active module: %w", err)
	}
	return workspace, module, nil
}

// LoadWithServicePathOverrideE resolves a service path without terminating the
// process, allowing long-running RunE commands to execute deferred teardown.
func LoadWithServicePathOverrideE(ctx context.Context, servicePath string) (*resources.Workspace, *resources.Module, *resources.Service, error) {
	workspace, err := LoadWorkspace(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	service, err := resources.LoadServiceFromDir(ctx, servicePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cannot load service: %w", err)
	}
	if workspace.Layout == resources.LayoutKindFlat {
		module, err := workspace.LoadModuleFromName(ctx, workspace.Name)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot load module: %w", err)
		}
		service.WithModule(module.Name)
		return workspace, module, service, nil
	}
	return nil, nil, nil, fmt.Errorf("cannot load a service path override in a non-flat workspace")
}
