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
	"strings"

	"github.com/codefly-dev/core/resources"
	"golang.org/x/mod/modfile"
)

const (
	GenericGoPluginVersion     = "0.0.16"
	GenericPythonPluginVersion = "0.0.16"
	NextJSPluginVersion        = "0.0.114"
	RustPluginVersion          = "0.0.18"
	SwiftPluginVersion         = "0.0.13"
	pythonSetupMarker          = "setup.py"
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
		{marker: pythonSetupMarker, name: "python", version: GenericPythonPluginVersion},
		{marker: "setup.cfg", name: "python", version: GenericPythonPluginVersion},
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
	// Markerless single-file repositories are common during editing and in
	// generated worktrees. Extension evidence is sufficient to select the
	// language plugin; the plugin remains authoritative for tool execution.
	var selected *resources.Agent
	err := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != sourceDir && skipDetectionDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		var name, version string
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".go":
			name, version = "go", GenericGoPluginVersion
		case ".py":
			name, version = "python", GenericPythonPluginVersion
		case ".rs":
			name, version = "rust", RustPluginVersion
		case ".swift":
			name, version = "swift", SwiftPluginVersion
		case ".js", ".jsx", ".ts", ".tsx":
			name, version = "nextjs", NextJSPluginVersion
		}
		if name != "" {
			selected = &resources.Agent{
				Kind: resources.ServiceAgent, Publisher: "codefly.dev",
				Name: name, Version: version,
			}
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect source files: %w", err)
	}
	if selected != nil {
		return selected, nil
	}
	return nil, fmt.Errorf("no Codefly source plugin is registered for %s", sourceDir)
}

func skipDetectionDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "target", "dist", "build", "__pycache__":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
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
	return prepare(ctx, absoluteSource, plugin)
}

// PrepareWithAgent creates an ephemeral source workspace using an explicitly
// selected agent. The caller must derive that choice from typed Codefly policy
// (for example, an explicit test formula), not from adapter-side commands.
func PrepareWithAgent(ctx context.Context, sourceDir string, plugin *resources.Agent) (*Prepared, error) {
	absoluteSource, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve source directory: %w", err)
	}
	if plugin == nil || plugin.Name == "" {
		return nil, fmt.Errorf("source agent is required")
	}
	return prepare(ctx, absoluteSource, plugin)
}

func prepare(ctx context.Context, absoluteSource string, plugin *resources.Agent) (*Prepared, error) {
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
		spec["with-workspace"] = goWorkspaceIncludes(absoluteSource)
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

func goWorkspaceIncludes(sourceDir string) bool {
	workFile := nearestFile(sourceDir, "go.work")
	if workFile == "" {
		return false
	}
	data, err := os.ReadFile(workFile)
	if err != nil {
		return false
	}
	parsed, err := modfile.ParseWork(workFile, data, nil)
	if err != nil {
		return false
	}
	sourcePhysical, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		sourcePhysical = filepath.Clean(sourceDir)
	}
	workDir := filepath.Dir(workFile)
	for _, use := range parsed.Use {
		moduleDir := use.Path
		if !filepath.IsAbs(moduleDir) {
			moduleDir = filepath.Join(workDir, moduleDir)
		}
		modulePhysical, evalErr := filepath.EvalSymlinks(moduleDir)
		if evalErr != nil {
			modulePhysical = filepath.Clean(moduleDir)
		}
		if modulePhysical == sourcePhysical {
			return true
		}
	}
	return false
}

func nearestFile(start, name string) string {
	current := filepath.Clean(start)
	for {
		candidate := filepath.Join(current, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}
