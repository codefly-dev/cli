package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Masterminds/semver"
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
		{
			name:      "git escape hatch names a strategy, not a location",
			ref:       &resources.ModuleReference{Name: "saas", Source: "owner/repo"},
			directive: &resources.ModuleResolveDirective{Git: true},
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

// writeGitFallbackOverlay writes a codefly.local.yaml opting the named modules
// out of verified resolution back to the unverified git clone. Verified
// resolution needs a `module-trust` policy these lightweight fixtures don't
// declare, so tests exercising the clone mechanics opt in explicitly; a real
// workspace without module-trust errors instead (TestRunFallsBackToGitOnlyWhenOptedIn).
func writeGitFallbackOverlay(t *testing.T, dir string, names ...string) {
	t.Helper()
	var buf strings.Builder
	buf.WriteString("resolve:\n")
	for _, name := range names {
		buf.WriteString("  " + name + ":\n    git: true\n")
	}
	if err := os.WriteFile(filepath.Join(dir, resources.LocalOverlayConfigurationName), []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addGitFallbackEntry appends a `git: true`-only resolve entry for name to an
// already-existing codefly.local.yaml in dir (written by resources.SaveLocalOverlay,
// hence the 4-space indent that marshal produces), without disturbing its
// other entries.
func addGitFallbackEntry(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, resources.LocalOverlayConfigurationName)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content = append(content, '\n')
	}
	content = append(content, []byte("    "+name+":\n        git: true\n")...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializePinnedModulesPullsAndWritesOverlay(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "module", "v0.0.1")

	workspaceDir := t.TempDir()
	writeGitFallbackOverlay(t, workspaceDir, "saas")
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
	writeGitFallbackOverlay(t, workspaceDir, "saas")
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

// A semver-range pin resolves to the highest published tag that satisfies it,
// not verbatim (which would clone a branch named ">=0.0.49" and fail).
func TestResolvePinnedTagResolvesSemverRange(t *testing.T) {
	source := initModuleRepo(t, "", "v0.0.48", "v0.0.49", "v0.0.50")
	sourceCache := t.TempDir()

	tag, err := resolvePinnedTag(context.Background(), source, ">=0.0.49", sourceCache)
	if err != nil {
		t.Fatalf("resolve range: %v", err)
	}
	if tag != "v0.0.50" {
		t.Fatalf(">=0.0.49 resolved to %q, want highest satisfying v0.0.50", tag)
	}
}

// An unsatisfiable range errors clearly, naming the constraint, rather than being
// passed verbatim to git.
func TestResolvePinnedTagUnsatisfiableRangeErrors(t *testing.T) {
	source := initModuleRepo(t, "", "v0.0.48", "v0.0.50")
	sourceCache := t.TempDir()

	_, err := resolvePinnedTag(context.Background(), source, ">=1.0.0", sourceCache)
	if err == nil {
		t.Fatal("unsatisfiable range must error")
	}
	if !strings.Contains(err.Error(), ">=1.0.0") {
		t.Fatalf("error should name the constraint, got %q", err)
	}
}

// A reachable remote that publishes no satisfying tag must error even when the
// cache still holds a formerly-satisfying tag (e.g. one the remote has since
// yanked). The cache is a fallback for an unreachable remote, never a stand-in for
// a remote that has definitively answered "nothing matches".
func TestResolvePinnedTagUnsatisfiableRangeIgnoresStaleCache(t *testing.T) {
	source := initModuleRepo(t, "", "v0.0.48") // remote no longer publishes anything >=0.0.49
	sourceCache := filepath.Join(t.TempDir(), "modules", "owner", "repo")
	if err := os.MkdirAll(filepath.Join(sourceCache, "v0.0.50"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := resolvePinnedTag(context.Background(), source, ">=0.0.49", sourceCache)
	if err == nil {
		t.Fatal("reachable remote with no satisfying tag must error, not boot the stale cached v0.0.50")
	}
	if !strings.Contains(err.Error(), ">=0.0.49") {
		t.Fatalf("error should name the constraint, got %q", err)
	}
}

// A range pin whose remote is unreachable degrades to the highest cached tag that
// satisfies the constraint, ignoring cached tags outside the range.
func TestResolvePinnedTagRangeOfflineUsesCache(t *testing.T) {
	sourceCache := filepath.Join(t.TempDir(), "modules", "owner", "repo")
	for _, tag := range []string{"v0.0.1", "v0.0.49", "v0.0.50", "v0.1.0"} {
		if err := os.MkdirAll(filepath.Join(sourceCache, tag), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	deadURL := "file://" + filepath.Join(t.TempDir(), "does-not-exist")

	tag, err := resolvePinnedTag(context.Background(), deadURL, ">=0.0.49, <0.1.0", sourceCache)
	if err != nil {
		t.Fatalf("offline range must fall back to cache: %v", err)
	}
	if tag != "v0.0.50" {
		t.Fatalf("offline range = %q, want highest cached satisfying v0.0.50", tag)
	}
}

// latest prefers a stable release over a higher pre-release, and falls back to a
// pre-release only when no stable tag exists.
func TestHighestSemverTagPrefersStable(t *testing.T) {
	if got := highestSemverTag([]string{"v1.0.0", "v1.1.0-rc1", "v0.9.0"}, nil); got != "v1.0.0" {
		t.Fatalf("highest = %q, want stable v1.0.0 over rc", got)
	}
	if got := highestSemverTag([]string{"v1.1.0-rc1", "v1.1.0-rc2"}, nil); got != "v1.1.0-rc2" {
		t.Fatalf("with only pre-releases, highest = %q, want v1.1.0-rc2", got)
	}
	if got := highestSemverTag([]string{"main", "not-a-tag"}, nil); got != "" {
		t.Fatalf("no semver tags should yield empty, got %q", got)
	}
}

// A constraint filters tags before the stable/pre-release split, so a plain range
// never selects an out-of-range pre-release even when it is numerically highest —
// following semver constraint semantics rather than `latest`'s pre-release
// fallback.
func TestHighestSemverTagRangeExcludesPrerelease(t *testing.T) {
	constraint, err := semver.NewConstraint(">=0.0.49")
	if err != nil {
		t.Fatal(err)
	}
	if got := highestSemverTag([]string{"v0.0.49", "v0.0.50-rc1"}, constraint); got != "v0.0.49" {
		t.Fatalf("range = %q, want stable v0.0.49, not out-of-range pre-release v0.0.50-rc1", got)
	}
	if got := highestSemverTag([]string{"v0.0.50-rc1"}, constraint); got != "" {
		t.Fatalf("range with only an out-of-range pre-release = %q, want empty", got)
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
	writeGitFallbackOverlay(t, workspaceDir, "good", "broken")
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
	// "broken" keeps its pre-existing git-fallback marker entry (the CLI never
	// deletes a directive it did not itself write) but must gain no path: a
	// failed pull must not be recorded as resolved.
	if broken := overlay.Resolve["broken"]; broken != nil && broken.Path != "" {
		t.Fatalf("broken module must have no resolved path, got %+v", broken)
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
	// "blog" has no location override, so it is CLI-managed; opt it into the
	// git-clone fallback (verified resolution needs a module-trust policy this
	// lightweight fixture doesn't declare).
	addGitFallbackEntry(t, parent, "blog")

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

// Without an explicit `resolve.<name>.git: true` opt-out, a workspace that
// declares no module-trust must fail closed rather than silently trust
// whatever the tag points to today. With the opt-out, the unverified clone
// path is used and warned about.
func TestRunFallsBackToGitOnlyWhenOptedIn(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "", "v0.0.1")

	t.Run("no module-trust and no opt-out errors", func(t *testing.T) {
		workspaceDir := t.TempDir()
		workspace := &resources.Workspace{
			Name:    "wiki",
			Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Version: "v0.0.1"}},
		}
		workspace.WithDir(workspaceDir)

		if err := materializePinnedModules(context.Background(), workspace); err != nil {
			t.Fatalf("a failed pinned resolution must not abort materialize: %v", err)
		}
		overlay, err := resources.LoadLocalOverlay(context.Background(), workspaceDir)
		if err != nil {
			t.Fatal(err)
		}
		if overlay != nil && overlay.Resolve["saas"] != nil && overlay.Resolve["saas"].Path != "" {
			t.Fatalf("module must not resolve without module-trust or an opt-out: %+v", overlay.Resolve["saas"])
		}
	})

	t.Run("git opt-out uses the unverified clone", func(t *testing.T) {
		workspaceDir := t.TempDir()
		writeGitFallbackOverlay(t, workspaceDir, "saas")
		workspace := &resources.Workspace{
			Name:    "wiki",
			Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Version: "v0.0.1"}},
		}
		workspace.WithDir(workspaceDir)

		if err := materializePinnedModules(context.Background(), workspace); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		overlay, err := resources.LoadLocalOverlay(context.Background(), workspaceDir)
		if err != nil {
			t.Fatal(err)
		}
		if overlay == nil || overlay.Resolve["saas"] == nil || overlay.Resolve["saas"].Path == "" {
			t.Fatalf("opted-out module must resolve via the git clone: %+v", overlay)
		}
		if _, err := os.Stat(filepath.Join(overlay.Resolve["saas"].Path, resources.ModuleConfigurationName)); err != nil {
			t.Fatalf("cloned module dir missing %s: %v", resources.ModuleConfigurationName, err)
		}
	})
}

// Concurrent `codefly run` invocations against the same workspace (e.g. `run
// service` and `run job` triggering materialization at the same time) must
// not lose each other's overlay entries: each call loads the overlay,
// computes its own full desired resolve map, and writes the whole thing
// back, so without serialization the last writer's full-map write silently
// clobbers whatever an earlier concurrent writer had just added.
// A real interleaving of the underlying lost-update race is inherently
// timing-dependent (see TestWithFileLockSerializesConcurrentCallers in
// pkg/composition for a deterministic test of the primitive the fix relies
// on); this is the integration-level smoke test that concurrent `codefly
// run` invocations against the same workspace (e.g. `run service` and `run
// job` triggering materialization at the same time) at least converge on a
// complete, consistent overlay rather than crashing or racing the on-disk
// file (run with -race in CI).
func TestMaterializePinnedModulesConcurrentRunsConvergeOnAConsistentOverlay(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	const count = 8
	modules := make([]*resources.ModuleReference, count)
	for i := range count {
		source := initModuleRepo(t, "", "v0.0.1")
		modules[i] = &resources.ModuleReference{Name: fmt.Sprintf("mod%d", i), Source: source, Version: "v0.0.1"}
	}
	workspaceDir := t.TempDir()
	var overlayDoc strings.Builder
	overlayDoc.WriteString("resolve:\n")
	for i := range count {
		fmt.Fprintf(&overlayDoc, "  mod%d:\n    git: true\n", i)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, resources.LocalOverlayConfigurationName), []byte(overlayDoc.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace := &resources.Workspace{Name: "wiki", Modules: modules}
	workspace.WithDir(workspaceDir)

	var wg sync.WaitGroup
	errs := make([]error, count)
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = materializePinnedModules(context.Background(), workspace)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent materialize %d: %v", i, err)
		}
	}

	overlay, err := resources.LoadLocalOverlay(context.Background(), workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		name := fmt.Sprintf("mod%d", i)
		directive := overlay.Resolve[name]
		if directive == nil || directive.Path == "" {
			t.Fatalf("entry for %s was lost to concurrent writes: %+v", name, overlay.Resolve)
		}
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

func TestGitResolved(t *testing.T) {
	gitCacheRoot := filepath.Join(t.TempDir(), "modules")
	for _, tc := range []struct {
		name      string
		directive *resources.ModuleResolveDirective
		want      bool
	}{
		{name: "no directive"},
		{
			name:      "user opt-out",
			directive: &resources.ModuleResolveDirective{Git: true},
			want:      true,
		},
		{
			name:      "path this CLI cloned",
			directive: &resources.ModuleResolveDirective{Path: filepath.Join(gitCacheRoot, "owner", "repo", "v1.0.0")},
			want:      true,
		},
		{
			name:      "user checkout elsewhere",
			directive: &resources.ModuleResolveDirective{Path: filepath.Join(t.TempDir(), "checkout")},
		},
		{
			name:      "verified pin",
			directive: &resources.ModuleResolveDirective{Pinned: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitResolved(tc.directive, gitCacheRoot); got != tc.want {
				t.Fatalf("gitResolved = %v, want %v", got, tc.want)
			}
		})
	}
}

// writeSolutionWorkspace writes a workspace manifest composing a single
// source-referenced module, so the overlay materialization can be exercised
// through a real resources.Workspace load — the way `codefly run` sees it.
func writeSolutionWorkspace(t *testing.T, dir, source, version string) {
	t.Helper()
	manifest := fmt.Sprintf("name: wiki\nlayout: modules\nmodules:\n    - name: saas\n      source: %s\n      version: %s\n", source, version)
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The git-clone escape hatch must survive repeated runs. An overlay entry may
// select only one of path/worktree/pinned/git, so materialization replaces the
// user's `git: true` with the clone it produced rather than adding the path
// beside it — an entry carrying both is rejected by core on the next load. The
// clone remains the resolution strategy across runs: with no module-trust
// declared, verified resolution cannot resolve the bumped version at all, so a
// v0.0.2 checkout can only come from the git clone.
func TestMaterializePinnedModulesGitOptOutSurvivesRepeatedRuns(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "", "v0.0.1", "v0.0.2")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspace(t, workspaceDir, source, "v0.0.1")
	writeGitFallbackOverlay(t, workspaceDir, "saas")

	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if err = materializePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	directive := overlay.Resolve["saas"]
	if directive == nil || directive.Path == "" || directive.Git {
		t.Fatalf("overlay entry must select the clone path alone, got %+v", directive)
	}

	// The next run reloads the workspace and resolves through core: an entry
	// selecting both path and git fails validation here.
	workspace, err = resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	if err != nil {
		t.Fatalf("reload workspace: %v", err)
	}
	resolution, err := workspace.ResolveModule(ctx, workspace.Modules[0])
	if err != nil {
		t.Fatalf("resolve after materialize: %v", err)
	}
	if resolution.Kind != resources.ResolutionLocalPath || resolution.Dir != directive.Path {
		t.Fatalf("resolution = %+v, want the clone at %s", resolution, directive.Path)
	}

	// Bumping the composed version re-clones: the module is still resolved by
	// cloning, not by the verified package the workspace declares no trust for.
	writeSolutionWorkspace(t, workspaceDir, source, "v0.0.2")
	workspace, err = resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	if err != nil {
		t.Fatalf("reload workspace after bump: %v", err)
	}
	if err = materializePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("materialize after bump: %v", err)
	}
	overlay, err = resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatalf("load overlay after bump: %v", err)
	}
	bumped := overlay.Resolve["saas"]
	if bumped == nil || bumped.Git || filepath.Base(bumped.Path) != "v0.0.2" {
		t.Fatalf("bumped entry must be the v0.0.2 clone alone, got %+v", bumped)
	}
}

// An overlay already corrupted by the earlier materialization (both a cache
// `path` and the original `git: true`) is repaired on the next run rather than
// left to fail core's validator forever.
func TestMaterializePinnedModulesRepairsPathAndGitEntry(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "", "v0.0.1")
	ctx := context.Background()

	workspaceDir := t.TempDir()
	writeSolutionWorkspace(t, workspaceDir, source, "v0.0.1")
	corrupted := filepath.Join(pinnedModuleCacheRoot(), filepath.FromSlash(source), "v0.0.1")
	if err := resources.SaveLocalOverlay(ctx, workspaceDir, &resources.LocalOverlay{
		Resolve: map[string]*resources.ModuleResolveDirective{"saas": {Path: corrupted, Git: true}},
	}); err != nil {
		t.Fatal(err)
	}

	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if err = materializePinnedModules(ctx, workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	directive := overlay.Resolve["saas"]
	if directive == nil || directive.Git || directive.Path != corrupted {
		t.Fatalf("entry must be repaired to the clone path alone, got %+v", directive)
	}
}
