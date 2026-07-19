// Package sourceworkspace adapts a repository checkout into an ephemeral
// Codefly source resource. Local commands, agent validation, Mind/editor
// integrations, and CI can then use the ordinary plugin RPCs without learning
// the implementation language or its native commands.
package sourceworkspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/resources"
)

const (
	GenericGoPluginVersion     = "0.0.15"
	GenericPythonPluginVersion = "0.0.15"
	NextJSPluginVersion        = "0.0.114"
	RustPluginVersion          = "0.0.17"
	SwiftPluginVersion         = "0.0.13"
)

// Prepared is a loaded ephemeral workspace containing one source resource.
type Prepared struct {
	Workspace *resources.Workspace
	Module    *resources.Module
	Service   *resources.Service
	Dir       string
	cleanup   func() error
}

// Close removes the ephemeral resource declaration without touching source.
func (p *Prepared) Close() error {
	if p == nil || p.cleanup == nil {
		return nil
	}
	return p.cleanup()
}

// SelectPlugin returns the authoritative plugin for a checkout. This registry
// is intentionally typed and extensible; callers never select native commands.
func SelectPlugin(sourceDir string) (*resources.Agent, error) {
	candidates := []struct {
		marker  string
		name    string
		version string
	}{
		{marker: "go.mod", name: "go", version: GenericGoPluginVersion},
		{marker: "pyproject.toml", name: "python", version: GenericPythonPluginVersion},
		{marker: "uv.lock", name: "python", version: GenericPythonPluginVersion},
		{marker: "package.json", name: "nextjs", version: NextJSPluginVersion},
		{marker: "Cargo.toml", name: "rust", version: RustPluginVersion},
		{marker: "Package.swift", name: "swift", version: SwiftPluginVersion},
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(sourceDir, candidate.marker)); err == nil {
			return &resources.Agent{
				Kind: resources.ServiceAgent, Publisher: "codefly.dev",
				Name: candidate.name, Version: candidate.version,
			}, nil
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect %s source marker: %w", candidate.marker, err)
		}
	}
	return nil, fmt.Errorf("no Codefly source plugin is registered for %s", sourceDir)
}

// Prepare creates and loads a flat one-service workspace whose source path is
// a symlink to sourceDir. The service configuration selects semantic plugin
// behavior only; language/toolchain implementation remains inside the plugin.
func Prepare(ctx context.Context, sourceDir string) (*Prepared, error) {
	absoluteSource, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve source directory: %w", err)
	}
	plugin, err := SelectPlugin(absoluteSource)
	if err != nil {
		return nil, err
	}
	temporary, err := os.MkdirTemp("", "codefly-source-workspace-*")
	if err != nil {
		return nil, fmt.Errorf("create source workspace: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = os.RemoveAll(temporary)
		}
	}()

	workspaceDir := filepath.Join(temporary, "workspace")
	serviceDir := filepath.Join(workspaceDir, "services", "source")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		return nil, fmt.Errorf("create source service: %w", err)
	}
	workspace := &resources.Workspace{
		Name:   "source-workspace",
		Layout: resources.LayoutKindFlat,
		Services: []*resources.ServiceReference{{
			Name: "source",
		}},
	}
	workspace.WithDir(workspaceDir)
	if err := workspace.Save(ctx); err != nil {
		return nil, fmt.Errorf("write source workspace: %w", err)
	}
	spec := map[string]any{"source-dir": "code"}
	if plugin.Name == "go" {
		spec["with-workspace"] = true
	}
	service := &resources.Service{
		Name:    "source",
		Version: "0.0.0",
		Agent:   plugin,
		Spec:    spec,
	}
	service.WithDir(serviceDir)
	if err := service.Save(ctx); err != nil {
		return nil, fmt.Errorf("write source service: %w", err)
	}
	if err := os.Symlink(absoluteSource, filepath.Join(serviceDir, "code")); err != nil {
		return nil, fmt.Errorf("link source checkout: %w", err)
	}

	loaded, err := resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("load source workspace: %w", err)
	}
	module, err := loaded.RootModule(ctx)
	if err != nil {
		return nil, fmt.Errorf("load source module: %w", err)
	}
	loadedService, err := module.LoadServiceFromName(ctx, "source")
	if err != nil {
		return nil, fmt.Errorf("load source service: %w", err)
	}

	failed = false
	return &Prepared{
		Workspace: loaded,
		Module:    module,
		Service:   loadedService,
		Dir:       workspaceDir,
		cleanup:   func() error { return os.RemoveAll(temporary) },
	}, nil
}
