package common

import (
	"context"
	"fmt"
	"slices"
	"strings"

	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/tui"
)

type ActiveContext struct {
	Workspace *resources.Workspace
	Module    *resources.Module
	Service   *resources.Service
}

func LoadActiveContext(ctx context.Context) (*ActiveContext, error) {
	return loadActiveContext(ctx, true)
}

// LoadActiveContextNonInteractive resolves only context encoded by the
// current path or by a single-service workspace. It never opens a selector;
// headless callers receive an actionable ambiguity error instead of /dev/tty.
func LoadActiveContextNonInteractive(ctx context.Context) (*ActiveContext, error) {
	return loadActiveContext(ctx, false)
}

func loadActiveContext(ctx context.Context, interactive bool) (*ActiveContext, error) {
	active := &ActiveContext{}

	workspace, err := resources.FindWorkspaceUp(ctx)
	if err != nil {
		return nil, err
	}

	if workspace == nil {
		return nil, fmt.Errorf("no workspace found")
	}
	active.Workspace = workspace

	if workspace.Layout == resources.LayoutKindFlat {
		module, err := workspace.LoadModuleFromName(ctx, workspace.Name)
		if err != nil {
			return nil, err
		}
		active.Module = module
		service, err := resources.LoadServiceFromCurrentPath(ctx)
		if err != nil {
			return nil, err
		}
		active.Service = service
	} else {
		module, service, err := resources.LoadModuleAndServiceFromCurrentPath(ctx)
		if err != nil {
			return nil, err
		}

		active.Module = module
		active.Service = service
	}

	if active.Service == nil {
		if active.Module != nil && active.Module.ServiceEntry != "" {
			active.Service, err = active.Module.LoadServiceFromName(ctx, active.Module.ServiceEntry)
			if err != nil {
				return nil, fmt.Errorf("cannot load module service entry %q: %w", active.Module.ServiceEntry, err)
			}
		}
	}

	if active.Service == nil {
		active.Service, active.Module, err = autoResolveService(ctx, workspace, interactive)
		if err != nil {
			return nil, err
		}
	}

	return active, nil
}

// autoResolveService picks a service when the user isn't inside a service
// folder. If exactly one service exists, it's selected automatically.
// If multiple exist, an interactive prompt lets the user choose.
func autoResolveService(ctx context.Context, workspace *resources.Workspace, interactive bool) (*resources.Service, *resources.Module, error) {
	// A single-module workspace can declare its default runnable service in the
	// module manifest. Resolve it before considering the workspace-wide service
	// list so headless runs remain deterministic.
	if workspace.Layout != resources.LayoutKindFlat && len(workspace.Modules) == 1 {
		module, err := workspace.LoadModuleFromReference(ctx, workspace.Modules[0])
		if err != nil {
			return nil, nil, err
		}
		if module.ServiceEntry != "" {
			service, err := module.LoadServiceFromName(ctx, module.ServiceEntry)
			if err != nil {
				return nil, nil, fmt.Errorf("cannot load module service entry %q: %w", module.ServiceEntry, err)
			}
			return service, module, nil
		}
	}

	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(services) == 0 {
		return nil, nil, nil
	}

	var picked *resources.Service
	if len(services) == 1 {
		picked = services[0]
	} else {
		if !interactive {
			names := make([]string, 0, len(services))
			for _, service := range services {
				names = append(names, service.Name)
			}
			slices.Sort(names)
			return nil, nil, fmt.Errorf("multiple services found (%s); pass the service name explicitly", strings.Join(names, ", "))
		}
		entries := make([]*tui.Entry, len(services))
		for i, svc := range services {
			entries[i] = &tui.Entry{Identifier: svc.Name}
		}
		result, selectErr := tui.RunSelect("Multiple services found — pick one:", entries)
		if selectErr != nil {
			return nil, nil, selectErr
		}
		if result.Stopped {
			return nil, nil, fmt.Errorf("service selection cancelled")
		}
		if result.Entry == nil {
			return nil, nil, fmt.Errorf("service selection returned no entry")
		}
		for _, svc := range services {
			if svc.Name == result.Entry.Identifier {
				picked = svc
				break
			}
		}
	}
	if picked == nil {
		return nil, nil, nil
	}

	for _, modRef := range workspace.Modules {
		mod, loadErr := workspace.LoadModuleFromReference(ctx, modRef)
		if loadErr != nil {
			continue
		}
		for _, svcRef := range mod.ServiceReferences {
			if svcRef.Name == picked.Name {
				picked.WithModule(mod.Name)
				return picked, mod, nil
			}
		}
	}
	return picked, nil, nil
}

// LoadService returns the active service without terminating the process.
func LoadService(ctx context.Context) (*resources.Service, error) {
	active, err := LoadActiveContext(ctx)
	if err != nil {
		return nil, err
	}
	if active.Service == nil {
		return nil, fmt.Errorf("no service found")
	}
	return active.Service, nil
}

// LoadModule returns the active module without terminating the process.
//
// ARCHITECTURE: Module-only commands must resolve the module from the current
// path before asking for an active service. In particular, codefly add service
// is commonly run from a newly-created module that has no services yet. Going
// through LoadActiveContext in that case invokes the workspace-wide service
// picker, which both selects the wrong abstraction and requires a TTY in
// otherwise non-interactive commands.
func LoadModule(ctx context.Context) (*resources.Module, error) {
	workspace, err := LoadWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	if workspace.Layout == resources.LayoutKindFlat {
		return workspace.LoadModuleFromName(ctx, workspace.Name)
	}

	module, _, err := resources.LoadModuleAndServiceFromCurrentPath(ctx)
	if err != nil {
		return nil, err
	}
	if module != nil {
		return module, nil
	}

	// Outside a module directory, retain the existing active-service fallback:
	// selecting a service also identifies its owning module.
	active, err := LoadActiveContext(ctx)
	if err != nil {
		return nil, err
	}
	if active.Module == nil {
		return nil, fmt.Errorf("no module found")
	}
	return active.Module, nil
}

// LoadWorkspace returns the enclosing workspace without terminating the
// process. RunE commands should prefer this over Workspace/RequireWorkspace so
// Cobra and deferred cleanup can observe failures.
func LoadWorkspace(ctx context.Context) (*resources.Workspace, error) {
	workspace, err := resources.FindWorkspaceUp(ctx)
	if err != nil {
		return nil, err
	}
	if workspace == nil {
		return nil, fmt.Errorf("no workspace found")
	}
	return workspace, nil
}
