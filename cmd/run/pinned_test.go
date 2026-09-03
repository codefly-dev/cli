package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestPinnedManaged(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "modules")
	inCache := filepath.Join(cacheRoot, "owner", "repo", "v1.0.0")
	for _, tc := range []struct {
		name      string
		ref       *resources.ModuleReference
		directive *resources.ModuleResolveDirective
		want      bool
	}{
		{
			name: "committed identity, no overlay",
			ref:  &resources.ModuleReference{Name: "saas", Source: "owner/repo", Version: "latest"},
			want: true,
		},
		{
			name: "no source is a local/in-repo module",
			ref:  &resources.ModuleReference{Name: "wiki"},
			want: false,
		},
		{
			name: "committed path override wins",
			ref:  &resources.ModuleReference{Name: "saas", Source: "owner/repo", PathOverride: strptr("../saas")},
			want: false,
		},
		{
			name:      "explicit pinned directive",
			ref:       &resources.ModuleReference{Name: "saas", Source: "owner/repo"},
			directive: &resources.ModuleResolveDirective{Pinned: true},
			want:      true,
		},
		{
			name:      "user worktree override left alone",
			ref:       &resources.ModuleReference{Name: "saas", Source: "owner/repo"},
			directive: &resources.ModuleResolveDirective{Worktree: "owner/repo@main"},
			want:      false,
		},
		{
			name:      "user path override left alone",
			ref:       &resources.ModuleReference{Name: "saas", Source: "owner/repo"},
			directive: &resources.ModuleResolveDirective{Path: "/home/me/checkout"},
			want:      false,
		},
		{
			name:      "auto-managed cache path is refreshed",
			ref:       &resources.ModuleReference{Name: "saas", Source: "owner/repo"},
			directive: &resources.ModuleResolveDirective{Path: inCache},
			want:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinnedManaged(tc.ref, tc.directive, cacheRoot); got != tc.want {
				t.Fatalf("pinnedManaged = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnderDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cache")
	if !underDir(base, base) {
		t.Fatal("a directory is under itself")
	}
	if !underDir(base, filepath.Join(base, "a", "b")) {
		t.Fatal("nested path should be under base")
	}
	if underDir(base, filepath.Dir(base)) {
		t.Fatal("parent must not be under base")
	}
	if underDir(base, filepath.Join(filepath.Dir(base), "cache-sibling")) {
		t.Fatal("sibling with a shared prefix must not be under base")
	}
}

// initModuleRepo builds a local git repository holding a module at moduleSubpath
// (root when empty), tagged with each of tags. It returns a file:// URL usable as
// a committed source identity.
func initModuleRepo(t *testing.T, moduleSubpath string, tags ...string) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "config", "user.email", "pinned@example.invalid")
	runGit(t, repo, "config", "user.name", "Pinned Test")
	moduleDir := repo
	if moduleSubpath != "" {
		moduleDir = filepath.Join(repo, moduleSubpath)
		if err := os.MkdirAll(moduleDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(moduleDir, resources.ModuleConfigurationName), []byte("name: saas\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "module")
	for _, tag := range tags {
		runGit(t, repo, "-c", "tag.gpgSign=false", "tag", tag)
	}
	return "file://" + repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestMaterializePinnedModulesPullsAndWritesOverlay(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "module", "v0.0.1")

	workspaceDir := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Module: "module", Version: "v0.0.1"}},
	}
	workspace.WithDir(workspaceDir)

	if err := materializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	overlay, err := resources.LoadLocalOverlay(context.Background(), workspaceDir)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	directive := overlay.Resolve["saas"]
	if directive == nil || directive.Path == "" {
		t.Fatalf("overlay has no path directive for saas: %+v", overlay.Resolve)
	}
	if _, err := os.Stat(filepath.Join(directive.Path, resources.ModuleConfigurationName)); err != nil {
		t.Fatalf("cache module dir missing %s: %v", resources.ModuleConfigurationName, err)
	}

	gitignore, err := os.ReadFile(filepath.Join(workspaceDir, ".gitignore"))
	if err != nil || !strings.Contains(string(gitignore), resources.LocalOverlayConfigurationName) {
		t.Fatalf(".gitignore does not ignore the overlay: %q (%v)", gitignore, err)
	}

	// Idempotent: a second run does not re-clone (the sentinel survives) and does
	// not rewrite the overlay path.
	sentinel := filepath.Join(directive.Path, "sentinel")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := materializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("cache was re-cloned (sentinel gone): %v", err)
	}
	overlay2, _ := resources.LoadLocalOverlay(context.Background(), workspaceDir)
	if overlay2.Resolve["saas"].Path != directive.Path {
		t.Fatalf("overlay path changed on idempotent run: %q -> %q", directive.Path, overlay2.Resolve["saas"].Path)
	}
}

func TestMaterializePinnedModulesLatestResolvesHighestTag(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "", "v0.0.1", "v0.1.0", "v0.0.9")

	workspaceDir := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Version: "latest"}},
	}
	workspace.WithDir(workspaceDir)

	if err := materializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	overlay, err := resources.LoadLocalOverlay(context.Background(), workspaceDir)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	path := overlay.Resolve["saas"].Path
	if filepath.Base(path) != "v0.1.0" {
		t.Fatalf("latest resolved to %q, want the v0.1.0 checkout", path)
	}
}

func TestMaterializePinnedModulesRespectsUserOverride(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())

	workspaceDir := t.TempDir()
	userCheckout := t.TempDir()
	overlay := &resources.LocalOverlay{Resolve: map[string]*resources.ModuleResolveDirective{
		"saas": {Path: userCheckout},
	}}
	if err := resources.SaveLocalOverlay(context.Background(), workspaceDir, overlay); err != nil {
		t.Fatal(err)
	}

	workspace := &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: "owner/repo", Version: "latest"}},
	}
	workspace.WithDir(workspaceDir)

	// No network happens: the user's path override is respected, so nothing is
	// pulled and the overlay is left byte-for-byte as written.
	if err := materializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	loaded, err := resources.LoadLocalOverlay(context.Background(), workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Resolve["saas"].Path != userCheckout {
		t.Fatalf("user override was rewritten: %q", loaded.Resolve["saas"].Path)
	}
}

// A concrete pinned version already in the cache must resolve without any network:
// no ls-remote, no clone — so a warmed-up solution boots offline.
func TestEnsurePinnedArtifactConcreteVersionOffline(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "modules")
	source := "unreachable/host" // https://github.com/unreachable/host.git — never contacted
	checkout := filepath.Join(cacheRoot, filepath.FromSlash(source), "v0.0.1")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, resources.ModuleConfigurationName), []byte("name: host\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ref := &resources.ModuleReference{Name: "host", Source: source, Version: "v0.0.1"}
	dir, err := ensurePinnedArtifact(context.Background(), ref, cacheRoot)
	if err != nil {
		t.Fatalf("cached concrete version must resolve offline: %v", err)
	}
	if dir != checkout {
		t.Fatalf("resolved to %q, want cached checkout %q", dir, checkout)
	}
}

// A `latest` constraint whose remote is unreachable degrades to the highest tag
// already in the cache, rather than failing an otherwise-bootable solution.
func TestResolvePinnedTagLatestOfflineUsesCache(t *testing.T) {
	sourceCache := filepath.Join(t.TempDir(), "modules", "owner", "repo")
	for _, tag := range []string{"v0.0.1", "v0.2.0", "v0.1.0"} {
		if err := os.MkdirAll(filepath.Join(sourceCache, tag), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	deadURL := "file://" + filepath.Join(t.TempDir(), "does-not-exist")

	tag, err := resolvePinnedTag(context.Background(), deadURL, "latest", sourceCache)
	if err != nil {
		t.Fatalf("offline latest must fall back to cache: %v", err)
	}
	if tag != "v0.2.0" {
		t.Fatalf("offline latest = %q, want highest cached v0.2.0", tag)
	}
}

// latest prefers a stable release over a higher pre-release, and falls back to a
// pre-release only when no stable tag exists.
func TestHighestSemverTagPrefersStable(t *testing.T) {
	if got := highestSemverTag([]string{"v1.0.0", "v1.1.0-rc1", "v0.9.0"}); got != "v1.0.0" {
		t.Fatalf("highest = %q, want stable v1.0.0 over rc", got)
	}
	if got := highestSemverTag([]string{"v1.1.0-rc1", "v1.1.0-rc2"}); got != "v1.1.0-rc2" {
		t.Fatalf("with only pre-releases, highest = %q, want v1.1.0-rc2", got)
	}
	if got := highestSemverTag([]string{"main", "not-a-tag"}); got != "" {
		t.Fatalf("no semver tags should yield empty, got %q", got)
	}
}

// One unreachable composed module must not abort the whole run: the pullable
// module is materialized, the broken one is left unresolved (a warning, not a
// hard error) for core to report only if the run actually needs it.
func TestMaterializePinnedModulesBestEffortOnPullFailure(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	good := initModuleRepo(t, "", "v0.0.1")
	dead := "file://" + filepath.Join(t.TempDir(), "gone")

	workspaceDir := t.TempDir()
	workspace := &resources.Workspace{
		Name: "wiki",
		Modules: []*resources.ModuleReference{
			{Name: "good", Source: good, Version: "v0.0.1"},
			{Name: "broken", Source: dead, Version: "v9.9.9"},
		},
	}
	workspace.WithDir(workspaceDir)

	if err := materializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("a broken composed module must not abort materialize: %v", err)
	}
	overlay, err := resources.LoadLocalOverlay(context.Background(), workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if overlay == nil || overlay.Resolve["good"] == nil || overlay.Resolve["good"].Path == "" {
		t.Fatalf("pullable module was not materialized: %+v", overlay)
	}
	if overlay.Resolve["broken"] != nil {
		t.Fatalf("broken module must have no overlay entry, got %+v", overlay.Resolve["broken"])
	}
}

// A directive in an ancestor codefly.local.yaml (the shared-monorepo layout) must
// be honored: a module the user pins to a worktree/path higher up is left alone,
// not silently re-pulled and shadowed by a fresh workspace-local overlay. Writes
// go back to the ancestor file so its other entries survive.
func TestMaterializePinnedModulesHonorsAncestorOverlay(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	parent := t.TempDir()
	workspaceDir := filepath.Join(parent, "solution")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userWorktree := "owner/editing@main"
	userPath := t.TempDir()
	ancestor := &resources.LocalOverlay{Resolve: map[string]*resources.ModuleResolveDirective{
		"editing": {Worktree: userWorktree},
		"host":    {Path: userPath},
	}}
	if err := resources.SaveLocalOverlay(context.Background(), parent, ancestor); err != nil {
		t.Fatal(err)
	}
	blog := initModuleRepo(t, "", "v0.0.1")

	workspace := &resources.Workspace{
		Name: "solution",
		Modules: []*resources.ModuleReference{
			{Name: "editing", Source: "owner/editing", Version: "latest"},
			{Name: "host", Source: "owner/host", Version: "latest"},
			{Name: "blog", Source: blog, Version: "v0.0.1"},
		},
	}
	workspace.WithDir(workspaceDir)

	if err := materializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// The write must land in the ancestor file, not a shadowing workspace-local one.
	if _, err := os.Stat(filepath.Join(workspaceDir, resources.LocalOverlayConfigurationName)); !os.IsNotExist(err) {
		t.Fatalf("a shadowing workspace-local overlay was created (err=%v)", err)
	}
	overlay, err := resources.LoadLocalOverlay(context.Background(), workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if overlay.Resolve["editing"].Worktree != userWorktree {
		t.Fatalf("ancestor worktree override lost/overwritten: %+v", overlay.Resolve["editing"])
	}
	if overlay.Resolve["host"].Path != userPath {
		t.Fatalf("ancestor path override lost/overwritten: %+v", overlay.Resolve["host"])
	}
	if overlay.Resolve["blog"] == nil || overlay.Resolve["blog"].Path == "" {
		t.Fatalf("pinned module was not materialized into the ancestor overlay: %+v", overlay.Resolve["blog"])
	}
}

// An auto-managed cache entry for a module no longer composed is pruned, so a
// removed dependency does not leave the overlay pointing at a stale checkout.
func TestPruneStalePinnedEntries(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "modules")
	resolve := map[string]*resources.ModuleResolveDirective{
		"gone":    {Path: filepath.Join(cacheRoot, "owner", "gone", "v1.0.0")},
		"kept":    {Path: filepath.Join(cacheRoot, "owner", "kept", "v1.0.0")},
		"user":    {Path: "/home/me/checkout"},
		"editing": {Worktree: "owner/editing@main"},
	}
	modules := []*resources.ModuleReference{{Name: "kept", Source: "owner/kept"}}

	if !pruneStalePinnedEntries(resolve, modules, cacheRoot) {
		t.Fatal("expected a prune to have happened")
	}
	if _, ok := resolve["gone"]; ok {
		t.Fatal("stale auto entry <gone> should have been pruned")
	}
	for _, name := range []string{"kept", "user", "editing"} {
		if _, ok := resolve[name]; !ok {
			t.Fatalf("entry <%s> must be preserved", name)
		}
	}
}
