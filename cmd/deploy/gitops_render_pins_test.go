package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// A developer's codefly.local.yaml points a composed module at a checkout. A
// render for any environment other than local resolves the module from its
// pin anyway and names the override it ignored; a local render keeps the
// checkout, as a run does. The file is untouched either way.
func TestRenderForANonLocalEnvironmentIgnoresTheLocalOverrideAndNamesIt(t *testing.T) {
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

	// A run materialized the pin; the developer then overrode it in place.
	_, pinned, err := loadGitOpsModule(ctx, []string{"saas"})
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(workspaceDir, "checkout")
	if err := os.CopyFS(checkout, os.DirFS(pinned.Dir())); err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(workspaceDir, resources.LocalOverlayConfigurationName)
	overlay := "resolve:\n    saas:\n        path: " + checkout + "\n"
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	cli.SetOutputSink(func(_ wool.Loglevel, msg string) { lines = append(lines, msg) })
	t.Cleanup(func() { cli.SetOutputSink(nil) })

	staging, err := forEnvironment(ctx, "staging")
	if err != nil {
		t.Fatalf("staging render: %v", err)
	}
	_, module, err := loadGitOpsModule(staging, []string{"saas"})
	if err != nil {
		t.Fatalf("staging render: %v", err)
	}
	if module.Dir() != pinned.Dir() || !strings.HasPrefix(module.Dir(), cacheRoot) {
		t.Fatalf("staging renders %s, want the pinned clone %s", module.Dir(), pinned.Dir())
	}
	want := "ignoring local override for saas: " + checkout + " (environment staging renders from pins)"
	if !strings.Contains(strings.Join(lines, "\n"), want) {
		t.Fatalf("the ignored override must be named; output:\n%s", strings.Join(lines, "\n"))
	}

	local, err := forEnvironment(ctx, "local")
	if err != nil {
		t.Fatalf("local render: %v", err)
	}
	workspace, module, err := loadGitOpsModule(local, []string{"saas"})
	if err != nil {
		t.Fatalf("local render: %v", err)
	}
	if module.Dir() != checkout {
		t.Fatalf("a local render keeps the override, got %s", module.Dir())
	}
	if overrides, err := composition.LocalOverrides(ctx, workspace); err != nil || len(overrides) != 1 || overrides[0] != (composition.LocalOverride{Module: "saas", Path: checkout}) {
		t.Fatalf("a local render honours the override a local inventory records as the source, got %+v %v", overrides, err)
	}
	after, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != overlay {
		t.Fatalf("the overlay must be left as the developer wrote it:\n%s", after)
	}
}
