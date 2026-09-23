package composition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
)

// writeSolutionWorkspaceWithResolution writes the committed manifest a
// workspace repository holds when it composes a module whose producer publishes
// no signed package yet: identity plus the declaration that it resolves from
// git. No overlay accompanies it — that is the whole point, the workspace says
// this on every machine.
func writeSolutionWorkspaceWithResolution(t *testing.T, dir, source, version, resolution string) {
	t.Helper()
	manifest := "name: wiki\nlayout: modules\n" + ModuleResolutionKey + ":\n    saas: " + resolution +
		"\nmodules:\n    - name: saas\n      source: " + source +
		"\n      version: " + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A workspace repository that contains nothing but workspace.codefly.yaml must
// be able to boot a module whose producer publishes no signed package, with no
// per-machine overlay step. The committed declaration is what says so, and
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

// Dropping the declaration once the producer publishes a package is meant to
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
// the module the default way: `saas: gti` must not read as "the producer
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

// A configured cache root is a directory the developer chose and may already
// keep their own checkouts in — the docs suggest pointing it at exactly such a
// tree. Containment in it is therefore not proof the CLI wrote a path, and
// reading it as proof let `run` overwrite a hand-written `resolve.<name>.path`
// aimed at a module they were editing. Ownership there rests on the receipt.
func TestMaterializePinnedModulesLeavesAUserCheckoutInsideTheConfiguredRoot(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	vendors := filepath.Join(t.TempDir(), "vendors")
	t.Setenv(ModuleCacheEnv, vendors)
	source := initModuleRepo(t, "", "v0.0.1")
	ctx := context.Background()

	userCheckout := filepath.Join(vendors, "my-saas-checkout")
	if err := os.MkdirAll(userCheckout, 0o755); err != nil {
		t.Fatal(err)
	}

	workspaceDir := t.TempDir()
	writeSolutionWorkspaceWithResolution(t, workspaceDir, source, "v0.0.1", "git")
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
	if got := overlay.Resolve["saas"].Path; got != userCheckout {
		t.Fatalf("the checkout the user is editing was replaced by %q; a root they chose is not proof of CLI ownership", got)
	}
}

// Under the *default* roots — which hold nothing but CLI output — containment
// must still establish ownership, or a pre-receipt materialization could never
// be reclaimed.
func TestMaterializePinnedModulesStillOwnsPathsUnderTheDefaultRoot(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "", "v0.0.1")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspaceWithResolution(t, workspaceDir, source, "v0.0.1", "git")
	// A materialization from before receipts existed: a cache path, no receipt.
	orphan := filepath.Join(mustCacheRoot(t), filepath.FromSlash(source), "v0.0.0")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := resources.SaveLocalOverlay(ctx, workspaceDir, &resources.LocalOverlay{
		Resolve: map[string]*resources.ModuleResolveDirective{"saas": {Path: orphan}},
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
	if got := overlay.Resolve["saas"].Path; got == orphan {
		t.Fatal("a path under a CLI-owned root must still be reclaimed without a receipt")
	}
}

// Moving the cache root must move the modules. Every run-shaped entry point goes
// through EnsurePinnedModules — only `run solution` re-resolves unconditionally
// — so a receipt that answers the request from the *previous* root would leave
// `run service` reporting success while handing core a directory that has moved
// or, once the old root is deleted, is not there at all.
func TestEnsurePinnedModulesRematerializesWhenTheCacheRootMoves(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	rootA := filepath.Join(t.TempDir(), "A")
	t.Setenv(ModuleCacheEnv, rootA)
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

	rootB := filepath.Join(t.TempDir(), "B")
	t.Setenv(ModuleCacheEnv, rootB)
	if err := os.RemoveAll(rootA); err != nil {
		t.Fatal(err)
	}

	if err := EnsurePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("a moved cache root must re-materialize, not fail: %v", err)
	}
	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	got := overlay.Resolve["saas"].Path
	if !underDir(rootB, got) {
		t.Fatalf("module still resolves to %q, outside the root in force now", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("the overlay names a materialization that is not there: %v", err)
	}
}

// The same gate covers a cache that was deleted without the root moving: the
// receipt still answers the request, but what it names is gone.
func TestEnsurePinnedModulesRematerializesWhenTheCacheIsDeleted(t *testing.T) {
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
	if err := os.RemoveAll(mustCacheRoot(t)); err != nil {
		t.Fatal(err)
	}

	if err := EnsurePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("a deleted cache must re-materialize, not fail: %v", err)
	}
	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overlay.Resolve["saas"].Path); err != nil {
		t.Fatalf("the overlay names a materialization that is not there: %v", err)
	}
}

// Adding the committed declaration to a module already on an overlay clone must
// not re-pull it. Both modes clone the same tag to the same path, so a mode flip
// there is pure churn: it invalidates the receipt, and on a cache miss would
// re-clone for a byte-identical result. The receipt's overlay-selected mode is
// consulted ahead of the declaration precisely so this does not happen, and this
// is what would catch that ordering being reversed.
func TestMaterializePinnedModulesDeclarationDoesNotChurnAnOverlayClone(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "", "v0.0.1")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspace(t, workspaceDir, source, "v0.0.1")
	writeGitFallbackOverlay(t, workspaceDir, "saas")
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Version: "v0.0.1"}},
	}
	workspace.WithDir(workspaceDir)
	if err := MaterializePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	before, err := LoadResolutionReceipts(workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if before["saas"].Mode != ResolutionModeGit {
		t.Fatalf("receipt mode = %q, want the overlay opt-out", before["saas"].Mode)
	}

	// The workspace now writes the opt-out down for everyone.
	writeSolutionWorkspaceWithResolution(t, workspaceDir, source, "v0.0.1", "git")

	answered, err := pinnedRequestsAnswered(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !answered {
		t.Fatal("the module is already on the clone the declaration asks for; re-materializing it is churn")
	}
	after, err := LoadResolutionReceipts(workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if after["saas"].Path != before["saas"].Path {
		t.Fatalf("the materialization moved from %q to %q", before["saas"].Path, after["saas"].Path)
	}
}

// In-flight clones stage under one reserved directory rather than as `.pull-*`
// siblings of the module trees, so the browsable layout a configured root exists
// for stays browsable — and a developer who must ignore the CLI's output has a
// bounded set of names to ignore.
func TestCloneStagesUnderTheReservedDirectory(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	vendors := filepath.Join(t.TempDir(), "vendors")
	t.Setenv(ModuleCacheEnv, vendors)
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

	if _, err := os.Stat(filepath.Join(vendors, stagingCacheDirName)); err != nil {
		t.Fatalf("clones did not stage under the reserved directory: %v", err)
	}
	entries, err := os.ReadDir(vendors)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pull-") {
			t.Fatalf("a staging directory was left beside the module trees: %q", entry.Name())
		}
	}
	// Everything the CLI owns under a configured root is one of the two reserved
	// names; the rest of the tree is the browsable layout the root exists for.
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") &&
			entry.Name() != stagingCacheDirName && entry.Name() != verifiedPackageCacheDirName {
			t.Fatalf("an unexpected reserved entry appeared in the browsable tree: %q", entry.Name())
		}
	}
}

// A stale staging directory is reclaimed wherever a previous version left it —
// under the reserved directory, and at the top level where `.pull-*` used to go.
// Litter that stopped being written but did not stop existing is still litter.
func TestSweepStalePullsReclaimsBothLayouts(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, ".pull-abc")
	current := filepath.Join(root, stagingCacheDirName, "pull-def")
	fresh := filepath.Join(root, stagingCacheDirName, "pull-ghi")
	for _, dir := range []string{old, current, fresh} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * time.Hour)
	for _, dir := range []string{old, current} {
		if err := os.Chtimes(dir, stale, stale); err != nil {
			t.Fatal(err)
		}
	}

	sweepStalePulls(root)

	for _, dir := range []string{old, current} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("stale staging directory %q was not reclaimed", dir)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a staging directory still in use was reclaimed: %v", err)
	}
}

// The CLI cannot write a .gitignore for a configured root without hiding the
// developer's own checkouts inside it, so it says so instead. The detection is
// what has to be right: it must find the enclosing repository from a directory
// that does not exist yet, and must not claim one that is not there.
func TestGitWorkTreeContaining(t *testing.T) {
	repository := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repository, "vendors", "modules")
	if got := gitWorkTreeContaining(nested); got != repository {
		t.Fatalf("gitWorkTreeContaining(%q) = %q, want %q — the root is named before it exists", nested, got, repository)
	}
	if got := gitWorkTreeContaining(repository); got != repository {
		t.Fatalf("the work tree root itself must resolve to itself, got %q", got)
	}
	if got := gitWorkTreeContaining(t.TempDir()); got != "" {
		t.Fatalf("a directory in no repository must resolve to nothing, got %q", got)
	}
}

// The read-only half of materialization is what `codefly doctor workspace`
// reports from, and ResolveComposedModuleDir is how the commands that read a
// composed module's tree get at it. Both must see a declared git resolution the
// same way `run` does: unmaterialized on a fresh workspace, materialized — into
// the clone cache, with no verified package involved — after one resolution,
// and unmaterialized again once the clone is gone from disk.
func TestUnmaterializedModulesAndResolveComposedModuleDirFollowADeclaredGitResolution(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	// The module sits at the repository root: the committed reference the
	// workspace file declares names no `module:` subpath.
	source := initModuleRepo(t, "", "v0.0.1")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspaceWithResolution(t, workspaceDir, source, "v0.0.1", "git")
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	if err != nil {
		t.Fatal(err)
	}

	missing, err := UnmaterializedModules(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].Name != "saas" || missing[0].MissingPath != "" {
		t.Fatalf("fresh workspace: unmaterialized = %+v, want saas with no path", missing)
	}

	dir, err := ResolveComposedModuleDir(ctx, workspace, workspace.Modules[0])
	if err != nil {
		t.Fatalf("a declared git resolution must materialize on first resolution: %v", err)
	}
	if !underDir(mustCacheRoot(t), dir) {
		t.Fatalf("resolved to %q, outside the clone cache", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, resources.ModuleConfigurationName)); err != nil {
		t.Fatalf("resolved directory is not a module: %v", err)
	}
	if missing, err = UnmaterializedModules(ctx, workspace); err != nil || len(missing) != 0 {
		t.Fatalf("after resolution: unmaterialized = %+v, %v; want none", missing, err)
	}

	// The clone vanished from under the overlay: the module is declared, was
	// materialized, and is not now — and the vanished path is named.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	missing, err = UnmaterializedModules(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].Name != "saas" || missing[0].MissingPath != dir {
		t.Fatalf("after deleting the clone: unmaterialized = %+v, want saas at %s", missing, dir)
	}
}
