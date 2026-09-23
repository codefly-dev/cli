package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
)

// A fresh checkout of a workspace that composes a module by source+version
// under a committed `module-resolution: <name>: git` declaration has no
// codefly.local.yaml. `deploy gitops render` must materialize that module
// exactly as `codefly run` does — clone into the module cache, record the
// receipt, point the overlay at the clone, gitignore both — and then load it,
// instead of relaying core's refusal to load a pinned module. This drives the
// loader every gitops verb goes through; the render itself needs an
// environment and a cluster-shaped module and is covered by the gitops tests.
func TestGitOpsRenderMaterializesADeclaredGitModuleOnAFreshWorkspace(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv(composition.ModuleCacheEnv, cacheRoot)
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initModuleRepo(t, "v0.0.1")

	workspaceDir := t.TempDir()
	manifest := "name: solution\nlayout: modules\n" + composition.ModuleResolutionKey + ":\n    saas: git\n" +
		"modules:\n    - name: saas\n      source: " + source + "\n      version: v0.0.1\n"
	if err := os.WriteFile(filepath.Join(workspaceDir, resources.WorkspaceConfigurationName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspaceDir)
	ctx := context.Background()

	workspace, module, err := loadGitOpsModule(ctx, []string{"saas"})
	if err != nil {
		t.Fatalf("a fresh workspace must materialize its declared module before loading it: %v", err)
	}
	if workspace.Dir() != workspaceDir {
		t.Fatalf("workspace dir = %q, want %q", workspace.Dir(), workspaceDir)
	}
	if module.Name != "saas" {
		t.Fatalf("module name = %q, want saas", module.Name)
	}
	if rel, relErr := filepath.Rel(cacheRoot, module.Dir()); relErr != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("module loaded from %q, want a clone under the module cache %q", module.Dir(), cacheRoot)
	}

	// What `run` leaves behind, and what the next command reads instead of
	// pulling again: the overlay path, its receipt, and both gitignored.
	overlay, err := resources.LoadLocalOverlay(ctx, workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	directive := overlay.Resolve["saas"]
	if directive == nil || directive.Path != module.Dir() {
		t.Fatalf("overlay does not select the materialized clone: %+v", directive)
	}
	receipts, err := composition.LoadResolutionReceipts(workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := receipts["saas"].Mode; got != composition.ResolutionModeDeclaredGit {
		t.Fatalf("receipt mode = %q, want %q", got, composition.ResolutionModeDeclaredGit)
	}
	ignored, err := os.ReadFile(filepath.Join(workspaceDir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{resources.LocalOverlayConfigurationName, composition.ResolutionRecordName} {
		if !strings.Contains(string(ignored), name+"\n") {
			t.Fatalf(".gitignore does not list %s:\n%s", name, ignored)
		}
	}
	missing, err := composition.UnmaterializedModules(ctx, workspace)
	if err != nil || len(missing) != 0 {
		t.Fatalf("after materialization nothing must be reported missing: %v %+v", err, missing)
	}

	// A second command on the same checkout is the CI-render-after-run case:
	// the request is answered, so nothing is pulled and the overlay is not
	// rewritten.
	before, err := os.Stat(filepath.Join(workspaceDir, resources.LocalOverlayConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if _, again, err := loadGitOpsModule(ctx, []string{"saas"}); err != nil || again.Dir() != module.Dir() {
		t.Fatalf("second load = %v, %v; want the same materialized module", again, err)
	}
	after, err := os.Stat(filepath.Join(workspaceDir, resources.LocalOverlayConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("an answered request must not rewrite the overlay")
	}
}

// initModuleRepo builds a local git repository holding a module at its root,
// tagged with each of tags, and returns a file:// URL usable as a committed
// source identity — the shape pkg/composition's own materialization tests use.
func initModuleRepo(t *testing.T, tags ...string) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "config", "user.email", "pinned@example.invalid")
	runGit(t, repo, "config", "user.name", "Pinned Test")
	if err := os.WriteFile(filepath.Join(repo, resources.ModuleConfigurationName), []byte("name: saas\n"), 0o644); err != nil {
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
