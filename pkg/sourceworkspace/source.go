// Package sourceworkspace adapts a repository checkout into an ephemeral
// Codefly source resource. Local commands, agent validation, Mind/editor
// integrations, and CI can then use the ordinary plugin RPCs without learning
// the implementation language or its native commands.
package sourceworkspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/discovery"
	"github.com/codefly-dev/core/resources"
	"golang.org/x/mod/modfile"
)

// Prepared is a loaded ephemeral workspace containing one source resource.
type Prepared struct {
	Workspace *resources.Workspace
	Module    *resources.Module
	Service   *resources.Service
	Dir       string
	// GoWorkFile is the absolute, ephemeral workspace file normalized from the
	// original Go checkout. Callers executing from the attached source must pass
	// it explicitly because the Go tool does not discover workspaces through a
	// symlinked working directory.
	GoWorkFile string
	// GoWorkSource is the developer's go.work GoWorkFile was normalized from:
	// the workspace that governs the source. Empty when the build is pinned.
	GoWorkSource string
	// GoWorkUnused is the nearest go.work above the source when it does NOT
	// `use` it: Go does not apply it, so the source builds pinned although a
	// workspace sits above it.
	GoWorkUnused string
	cleanup      func() error
}

// Close removes the ephemeral resource declaration without touching source.
func (p *Prepared) Close() error {
	if p == nil || p.cleanup == nil {
		return nil
	}
	return p.cleanup()
}

// SelectPlugin selects from runtime advertisements, never release pins.
func SelectPlugin(ctx context.Context, sourceDir string) (*resources.Agent, error) {
	return selectPlugin(ctx, sourceDir)
}

// Prepare creates and loads a flat one-service workspace whose source path is
// a symlink to sourceDir. The service configuration selects semantic plugin
// behavior only; language/toolchain implementation remains inside the plugin.
func Prepare(ctx context.Context, sourceDir string) (*Prepared, error) {
	absoluteSource, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve source directory: %w", err)
	}
	plugin, err := selectPlugin(ctx, absoluteSource)
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
	if err = os.MkdirAll(serviceDir, 0o755); err != nil {
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
	if err = workspace.Save(ctx); err != nil {
		return nil, fmt.Errorf("write source workspace: %w", err)
	}
	spec := map[string]any{"source-dir": "code"}
	goWorkFile, goWorkSource, goWorkUnused := "", "", ""
	if _, statErr := os.Stat(filepath.Join(absoluteSource, "go.mod")); statErr == nil {
		if goWorkSource = goWorkspaceFile(absoluteSource); goWorkSource != "" {
			goWorkFile = filepath.Join(workspaceDir, "go.work")
			if err = writeNormalizedGoWorkspace(goWorkSource, goWorkFile); err != nil {
				return nil, fmt.Errorf("prepare Go workspace: %w", err)
			}
		} else {
			goWorkUnused = goWorkspaceUnused(absoluteSource)
		}
		spec["with-workspace"] = goWorkFile != ""
	}
	service := &resources.Service{
		Name:    "source",
		Version: "0.0.0",
		Agent:   plugin,
		Spec:    spec,
	}
	service.WithDir(serviceDir)
	if err = service.Save(ctx); err != nil {
		return nil, fmt.Errorf("write source service: %w", err)
	}
	if err = os.Symlink(absoluteSource, filepath.Join(serviceDir, "code")); err != nil {
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
		Workspace:    loaded,
		Module:       module,
		Service:      loadedService,
		Dir:          workspaceDir,
		GoWorkFile:   goWorkFile,
		GoWorkSource: goWorkSource,
		GoWorkUnused: goWorkUnused,
		cleanup:      func() error { return os.RemoveAll(temporary) },
	}, nil
}

func goWorkspaceIncludes(sourceDir string) bool {
	return goWorkspaceFile(sourceDir) != ""
}

// goWorkspaceUnused is the nearest go.work above sourceDir when it does not
// govern it (it does not `use` the module, or cannot be read as a workspace):
// the file a developer would expect to apply, and Go does not.
func goWorkspaceUnused(sourceDir string) string {
	if goWorkspaceFile(sourceDir) != "" {
		return ""
	}
	workFile := nearestFile(sourceDir, "go.work")
	if workFile == "" {
		return ""
	}
	if physical, err := filepath.EvalSymlinks(workFile); err == nil {
		return physical
	}
	return filepath.Clean(workFile)
}

func goWorkspaceFile(sourceDir string) string {
	workFile := nearestFile(sourceDir, "go.work")
	if workFile == "" {
		return ""
	}
	data, err := os.ReadFile(workFile)
	if err != nil {
		return ""
	}
	parsed, err := modfile.ParseWork(workFile, data, nil)
	if err != nil {
		return ""
	}
	workPhysical, err := filepath.EvalSymlinks(workFile)
	if err != nil {
		workPhysical = filepath.Clean(workFile)
	}
	sourcePhysical, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		sourcePhysical = filepath.Clean(sourceDir)
	}
	workDir := filepath.Dir(workPhysical)
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
			return workPhysical
		}
	}
	return ""
}

// writeNormalizedGoWorkspace copies the governing workspace into the
// ephemeral source resource while resolving its filesystem members and local
// replacements to physical paths. This prevents host aliases such as
// /tmp→/private/tmp and the attachment symlink from changing Go's module
// identity. The developer's workspace remains untouched.
//
// A member or local replacement that is not on disk is refused, naming the
// workspace and the member, as Go itself refuses that workspace. Omitting it
// would build against the pinned version of the module the developer linked
// locally, and report success.
func writeNormalizedGoWorkspace(source, destination string) error {
	payload, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	workspace, err := modfile.ParseWork(source, payload, nil)
	if err != nil {
		return err
	}
	sourceDirectory := filepath.Dir(source)
	uses := make([]*modfile.Use, 0, len(workspace.Use))
	seenUses := make(map[string]struct{}, len(workspace.Use))
	for _, use := range workspace.Use {
		physical, err := physicalWorkspacePath(sourceDirectory, use.Path)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: use %s does not exist; restore that checkout, or remove the member (`go work edit -dropuse %s`): Go refuses this workspace, and building without it would silently use the pinned version instead", source, use.Path, use.Path)
		}
		if err != nil {
			return fmt.Errorf("resolve use %s: %w", use.Path, err)
		}
		normalized := filepath.ToSlash(physical)
		if _, duplicate := seenUses[normalized]; duplicate {
			continue
		}
		seenUses[normalized] = struct{}{}
		uses = append(uses, &modfile.Use{Path: normalized, ModulePath: use.ModulePath})
	}
	// modfile.SetUse appends every requested path after retaining already
	// matching entries. When an existing use is already an absolute physical
	// path, that produces the same module twice and Go rejects the generated
	// workspace. Remove the source entries first, then add the canonical set.
	for _, use := range append([]*modfile.Use(nil), workspace.Use...) {
		if err := workspace.DropUse(use.Path); err != nil {
			return err
		}
	}
	for _, use := range uses {
		if err := workspace.AddUse(use.Path, use.ModulePath); err != nil {
			return err
		}
	}

	type localReplacement struct {
		oldPath, oldVersion, newPath string
	}
	var replacements []localReplacement
	for _, replacement := range workspace.Replace {
		if replacement.New.Version != "" {
			continue
		}
		candidate := localReplacement{
			oldPath: replacement.Old.Path, oldVersion: replacement.Old.Version,
		}
		physical, err := physicalWorkspacePath(sourceDirectory, replacement.New.Path)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: replace %s => %s does not exist; restore that checkout, or remove the replacement: building without it would silently use the pinned version instead", source, replacement.Old.Path, replacement.New.Path)
		}
		if err != nil {
			return fmt.Errorf("resolve replacement %s: %w", replacement.New.Path, err)
		}
		candidate.newPath = filepath.ToSlash(physical)
		replacements = append(replacements, candidate)
	}
	for _, replacement := range replacements {
		if err := workspace.DropReplace(replacement.oldPath, replacement.oldVersion); err != nil {
			return err
		}
		if err := workspace.AddReplace(replacement.oldPath, replacement.oldVersion, replacement.newPath, ""); err != nil {
			return err
		}
	}
	workspace.Cleanup()
	workspace.SortBlocks()
	// The workspace file is written from its own directory.
	directory, openErr := os.OpenRoot(filepath.Dir(destination))
	if openErr != nil {
		return openErr
	}
	defer func() { _ = directory.Close() }()
	return directory.WriteFile(filepath.Base(destination), modfile.Format(workspace.Syntax), 0o600)
}

func physicalWorkspacePath(base, value string) (string, error) {
	path := filepath.FromSlash(value)
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return physical, nil
}

func nearestFile(start, name string) string {
	for _, current := range discovery.Ancestors(start, discovery.Ceilings) {
		candidate := filepath.Join(current, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}
