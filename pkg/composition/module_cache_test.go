package composition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// writeSolutionWorkspaceWithResolution writes the committed manifest a
// workspace repository holds when it composes a module whose producer publishes
// no signed package yet: identity plus the declaration that it resolves from
// git. No overlay accompanies it — that is the whole point, the workspace says
// this on every machine.
func writeSolutionWorkspaceWithResolution(t *testing.T, dir, source, version, resolution string) {
	t.Helper()
	manifest := "name: wiki\nlayout: modules\nmodules:\n    - name: saas\n      source: " + source +
		"\n      version: " + version + "\n      resolution: " + resolution + "\n"
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A workspace repository that contains nothing but workspace.codefly.yaml must
// be able to boot a module whose producer publishes no signed package, with no
// per-machine overlay step. The committed `resolution: git` is what says so, and
// what `run` must act on exactly as it acts on the overlay opt-out.
func TestMaterializePinnedModulesHonorsCommittedGitResolution(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "module", "v0.0.1")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspaceWithResolution(t, workspaceDir, source, "v0.0.1", "git")
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Module: "module", Version: "v0.0.1"}},
	}
	workspace.WithDir(workspaceDir)

	if err := MaterializePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("a declared git resolution must materialize with no overlay: %v", err)
	}

	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	directive := overlay.Resolve["saas"]
	if directive == nil || directive.Path == "" {
		t.Fatalf("overlay has no materialized path for saas: %+v", overlay.Resolve)
	}
	if directive.Git {
		t.Fatal("the declaration lives in the committed manifest; run must not forge a machine-local opt-out beside it")
	}
	if !underDir(mustCacheRoot(t), directive.Path) {
		t.Fatalf("declared git resolution landed at %q, outside the clone cache", directive.Path)
	}
	if _, err := os.Stat(filepath.Join(directive.Path, resources.ModuleConfigurationName)); err != nil {
		t.Fatalf("materialized directory is not a module: %v", err)
	}

	receipts, err := LoadResolutionReceipts(workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := receipts["saas"].Mode; got != ResolutionModeDeclaredGit {
		t.Fatalf("receipt mode = %q, want %q — the receipt must record who asked for the clone", got, ResolutionModeDeclaredGit)
	}

	// Steady state: the request is answered, so a later run resolves it with no
	// network and without taking the overlay lock.
	if err := EnsurePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("a materialized declaration must answer its own request: %v", err)
	}
}

// Dropping `resolution: git` once the producer publishes a package is meant to
// be the whole edit. The receipt still records a clone, so if the receipt were
// consulted ahead of the (now absent) declaration the module would silently keep
// cloning forever; here the workspace declares no module-trust either, so the
// return to verified resolution is visible as the failure it should be.
func TestMaterializePinnedModulesDroppingTheDeclarationReturnsToVerified(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "", "v0.0.1")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspaceWithResolution(t, workspaceDir, source, "v0.0.1", "git")
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Version: "v0.0.1"}},
	}
	workspace.WithDir(workspaceDir)
	if err := MaterializePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	writeSolutionWorkspace(t, workspaceDir, source, "v0.0.1")
	err := MaterializePinnedModules(ctx, workspace)
	if err == nil {
		t.Fatal("without the declaration the module resolves verified, which this workspace cannot do")
	}
	if !strings.Contains(err.Error(), "module-trust") {
		t.Fatalf("the failure must name verified resolution's missing precondition: %v", err)
	}
	overlay, loadErr := resources.LoadLocalOverlay(ctx, workspaceDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if entry := overlay.Resolve["saas"]; entry != nil {
		t.Fatalf("the clone answers no current request and must be dropped, got %+v", entry)
	}
}

// An overlay directive is the user speaking on this machine, so it still wins
// over the committed declaration — including `pinned: true`, the way a developer
// with module-trust set up tests verified resolution of a module the workspace
// still declares as git.
func TestMaterializePinnedModulesOverlayOverridesTheDeclaration(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "", "v0.0.1")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspaceWithResolution(t, workspaceDir, source, "v0.0.1", "git")
	userCheckout := t.TempDir()
	if err := resources.SaveLocalOverlay(ctx, workspaceDir, &resources.LocalOverlay{
		Resolve: map[string]*resources.ModuleResolveDirective{"saas": {Path: userCheckout}},
	}); err != nil {
		t.Fatal(err)
	}
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Version: "v0.0.1"}},
	}
	workspace.WithDir(workspaceDir)

	if err := MaterializePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if overlay.Resolve["saas"].Path != userCheckout {
		t.Fatalf("a declaration must not clobber the checkout the user is editing: %q", overlay.Resolve["saas"].Path)
	}
}

// A malformed declaration fails the whole materialization rather than resolving
// the module the default way: `resolution: gti` must not read as "the producer
// publishes a signed package".
func TestMaterializePinnedModulesRejectsUnknownResolution(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	workspaceDir := t.TempDir()
	writeSolutionWorkspaceWithResolution(t, workspaceDir, "owner/saas", "v0.0.1", "gti")
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: "owner/saas", Version: "v0.0.1"}},
	}
	workspace.WithDir(workspaceDir)

	err := MaterializePinnedModules(context.Background(), workspace)
	if err == nil || !strings.Contains(err.Error(), "gti") {
		t.Fatalf("an unsupported resolution must be refused by name, got %v", err)
	}
}

// The cache root is where a developer goes looking for the modules their
// workspace composes, so it is theirs to choose — and both caches must follow
// it, or the verified ones stay inside the workspace repository that is supposed
// to contain no module bytes at all.
func TestModuleCacheRootFollowsTheEnvironment(t *testing.T) {
	home := t.TempDir()
	workspaceDir := t.TempDir()

	t.Run("default", func(t *testing.T) {
		t.Setenv(resources.CodeflyHomeEnv, home)
		if got := mustCacheRoot(t); got != filepath.Join(home, "modules") {
			t.Fatalf("clone cache root = %q", got)
		}
		if got := mustVerifiedCacheRoot(t, workspaceDir); got != filepath.Join(workspaceDir, ".codefly", "cache", "modules") {
			t.Fatalf("verified cache root = %q", got)
		}
	})

	t.Run("configured", func(t *testing.T) {
		vendors := filepath.Join(t.TempDir(), "vendors")
		t.Setenv(ModuleCacheEnv, vendors)
		if got := mustCacheRoot(t); got != vendors {
			t.Fatalf("clone cache root = %q, want the configured root", got)
		}
		if got := mustVerifiedCacheRoot(t, workspaceDir); got != filepath.Join(vendors, verifiedPackageCacheDirName) {
			t.Fatalf("verified cache root = %q, want it beside the clones", got)
		}
	})

	t.Run("home-relative", func(t *testing.T) {
		t.Setenv(ModuleCacheEnv, "~/development/vendors")
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home directory on this machine")
		}
		if got := mustCacheRoot(t); got != filepath.Join(userHome, "development", "vendors") {
			t.Fatalf("clone cache root = %q, want ~ expanded", got)
		}
	})

	t.Run("relative is refused, never silently defaulted", func(t *testing.T) {
		t.Setenv(ModuleCacheEnv, "vendors")
		if _, err := pinnedModuleCacheRoot(); err == nil {
			t.Fatal("a relative cache root would put the modules somewhere other than where the developer said")
		}
	})
}

// End to end: with a configured root, the module a workspace composes lands at
// <root>/<owner>/<repo>/<tag>/<module subpath> — the layout a developer browses.
func TestMaterializePinnedModulesUsesTheConfiguredCacheRoot(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	vendors := filepath.Join(t.TempDir(), "vendors")
	t.Setenv(ModuleCacheEnv, vendors)
	source := initModuleRepo(t, "module", "v0.0.1")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspaceWithResolution(t, workspaceDir, source, "v0.0.1", "git")
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Module: "module", Version: "v0.0.1"}},
	}
	workspace.WithDir(workspaceDir)

	if err := MaterializePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(vendors, filepath.FromSlash(source), "v0.0.1", "module")
	if got := overlay.Resolve["saas"].Path; got != want {
		t.Fatalf("module landed at %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, ".codefly", "cache")); !os.IsNotExist(err) {
		t.Fatalf("a configured cache root must leave no module bytes in the workspace: %v", err)
	}
}
