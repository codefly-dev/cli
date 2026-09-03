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
