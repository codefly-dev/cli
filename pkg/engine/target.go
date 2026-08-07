package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// ServiceTarget identifies a service without relying on process-global state.
type ServiceTarget struct {
	// Name is optional when Root contains service.codefly.yaml. When provided,
	// it is verified against that manifest.
	Name string
	// Root is the service source directory. Relative roots are resolved against
	// the owning WorkspaceHost root, never against the process CWD.
	Root string
	// Agent is an optional service-agent identifier (for example
	// "go-generic:latest"). If empty, the service manifest selects the agent.
	Agent string
	// ForceSource binds Root as an exact source checkout even when an enclosing
	// Codefly service manifest exists. Code-unit dispatch uses this so a nested
	// unit cannot silently widen to its parent service boundary.
	ForceSource bool
}

type serviceDescriptor struct {
	target      ServiceTarget
	workspace   *resources.Workspace
	module      *resources.Module
	service     *resources.Service
	identity    *basev0.ServiceIdentity
	environment *resources.Environment
	cleanup     func() error
}

func normalizeTarget(hostRoot string, target ServiceTarget) (ServiceTarget, error) {
	root := strings.TrimSpace(target.Root)
	if root == "" {
		root = hostRoot
	} else if !filepath.IsAbs(root) {
		root = filepath.Join(hostRoot, root)
	}
	root = filepath.Clean(root)
	root, info, err := canonicalPath(root)
	if err != nil {
		return ServiceTarget{}, fmt.Errorf("resolve service root: %w", err)
	}
	if info != nil && !info.IsDir() {
		return ServiceTarget{}, fmt.Errorf("service root is not a directory: %s", root)
	}
	relative, err := filepath.Rel(hostRoot, root)
	if err != nil {
		return ServiceTarget{}, fmt.Errorf("resolve service root relative to host: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return ServiceTarget{}, fmt.Errorf("service root %q is outside workspace host root %q", root, hostRoot)
	}
	target.Root = root
	target.Name = strings.TrimSpace(target.Name)
	target.Agent = strings.TrimSpace(target.Agent)
	return target, nil
}

// canonicalPath resolves symlinks in the deepest existing ancestor while
// preserving a missing suffix. This keeps the host boundary physical (so a
// symlink cannot escape it) without forcing adapters to eagerly create a
// checkout that an operation may be expected to report as missing.
func canonicalPath(path string) (string, os.FileInfo, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		info, err := os.Stat(current)
		if err == nil {
			physical, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", nil, evalErr
			}
			for index := len(missing) - 1; index >= 0; index-- {
				physical = filepath.Join(physical, missing[index])
			}
			if len(missing) > 0 {
				info = nil
			}
			return filepath.Clean(physical), info, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil, err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func resolveServiceDescriptor(ctx context.Context, target ServiceTarget) (*serviceDescriptor, error) {
	if target.ForceSource {
		if target.Agent == "" {
			return nil, fmt.Errorf("forced source target requires an explicit agent")
		}
		return prepareSourceDescriptor(ctx, target)
	}
	workspaceDir, err := parentResourceDir(target.Root, resources.WorkspaceConfigurationName)
	if err != nil {
		if target.Agent != "" {
			return prepareSourceDescriptor(ctx, target)
		}
		return nil, err
	}
	serviceDir, err := parentResourceDir(target.Root, resources.ServiceConfigurationName)
	if err != nil {
		if target.Agent != "" {
			return prepareSourceDescriptor(ctx, target)
		}
		return nil, err
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	var module *resources.Module
	if workspace.Layout == resources.LayoutKindFlat {
		module, err = workspace.LoadModuleFromName(ctx, workspace.Name)
	} else {
		var moduleDir string
		moduleDir, err = parentResourceDir(target.Root, resources.ModuleConfigurationName)
		if err == nil {
			module, err = resources.LoadModuleFromDir(ctx, moduleDir)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("load enclosing module: %w", err)
	}
	if module == nil {
		return nil, fmt.Errorf("load enclosing module: no module resolved")
	}
	service, err := resources.LoadServiceFromDir(ctx, serviceDir)
	if err != nil {
		return nil, fmt.Errorf("load service: %w", err)
	}
	if target.Name != "" && target.Name != service.Name {
		return nil, fmt.Errorf("service target %q does not match manifest service %q", target.Name, service.Name)
	}
	service.WithModule(module.Name)
	relative, err := workspace.RelativeDir(service)
	if err != nil {
		return nil, fmt.Errorf("resolve service path relative to workspace: %w", err)
	}
	environment := resources.LocalEnvironment()
	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              module.Name,
		Workspace:           workspace.Name,
		WorkspacePath:       workspace.Dir(),
		RelativeToWorkspace: relative,
	}
	return &serviceDescriptor{
		target:      target,
		workspace:   workspace,
		module:      module,
		service:     service,
		identity:    identity,
		environment: environment,
	}, nil
}

func prepareSourceDescriptor(ctx context.Context, target ServiceTarget) (*serviceDescriptor, error) {
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, target.Agent)
	if err != nil {
		return nil, fmt.Errorf("parse source agent %q: %w", target.Agent, err)
	}
	prepared, err := sourceworkspace.PrepareWithAgent(ctx, target.Root, agent)
	if err != nil {
		return nil, fmt.Errorf("prepare source workspace: %w", err)
	}
	relative, err := prepared.Workspace.RelativeDir(prepared.Service)
	if err != nil {
		_ = prepared.Close()
		return nil, fmt.Errorf("resolve prepared source path: %w", err)
	}
	environment := resources.LocalEnvironment()
	return &serviceDescriptor{
		target:    target,
		workspace: prepared.Workspace,
		module:    prepared.Module,
		service:   prepared.Service,
		identity: &basev0.ServiceIdentity{
			Name:                prepared.Service.Name,
			Version:             prepared.Service.Version,
			Module:              prepared.Module.Name,
			Workspace:           prepared.Workspace.Name,
			WorkspacePath:       prepared.Workspace.Dir(),
			RelativeToWorkspace: relative,
		},
		environment: environment,
		cleanup:     prepared.Close,
	}, nil
}

func (d *serviceDescriptor) agentName() (string, error) {
	if d.target.Agent != "" {
		return d.target.Agent, nil
	}
	if d.service == nil || d.service.Agent == nil {
		return "", fmt.Errorf("service %q does not configure an agent", d.target.Name)
	}
	return d.service.Agent.String(), nil
}

func parentResourceDir(start, configurationName string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve %s search root: %w", configurationName, err)
	}
	for {
		info, statErr := os.Stat(filepath.Join(current, configurationName))
		if statErr == nil && !info.IsDir() {
			return current, nil
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspect %s: %w", configurationName, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("%s not found above %s", configurationName, start)
		}
		current = parent
	}
}
