package composition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// A developer's checkout override and a service override are local overrides;
// the path the CLI materialized a pinned module to is not. A render for a
// hosted environment resolves under a view with the overrides replaced by the
// pins — materialized if the override displaced them — while the file on disk
// keeps every override for the next local run.
func TestPinnedViewReplacesLocalOverridesWithPinsAndLeavesTheFileAlone(t *testing.T) {
	t.Setenv(ModuleCacheEnv, t.TempDir())
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	ctx := context.Background()
	source := initServiceModuleRepo(t, "gateway", "v0.0.1")
	billing := initModuleRepo(t, "", "v0.0.1")
	dir := t.TempDir()
	manifest := "name: solution\nlayout: modules\n" + ModuleResolutionKey + ":\n    saas: git\n    billing: git\n" +
		"modules:\n    - name: saas\n      source: " + source + "\n      module: module\n      version: v0.0.1\n" +
		"    - name: billing\n      source: " + billing + "\n      version: v0.0.1\n"
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	// What a run leaves behind: both modules materialized, paths and receipts.
	if err := MaterializePinnedModules(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if overrides, err := LocalOverrides(ctx, workspace); err != nil || len(overrides) != 0 {
		t.Fatalf("a materialized pin is not a local override: %v %+v", err, overrides)
	}

	// The developer then points saas at a checkout, and one service of billing
	// at another; billing itself stays on the path the CLI wrote.
	checkout := filepath.Join(dir, "checkout")
	overlay, err := resources.LoadLocalOverlay(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(checkout, os.DirFS(overlay.Resolve["saas"].Path)); err != nil {
		t.Fatal(err)
	}
	pinnedBilling := overlay.Resolve["billing"].Path
	overlay.Resolve["saas"] = &resources.ModuleResolveDirective{Path: checkout}
	overlay.Resolve["billing"].Services = map[string]*resources.ServiceResolveDirective{"api": {Path: filepath.Join(checkout, "services", "gateway")}}
	if err := resources.SaveLocalOverlay(ctx, dir, overlay); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, resources.LocalOverlayConfigurationName))
	if err != nil {
		t.Fatal(err)
	}

	workspace, err = resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	filter, ignored, err := PinnedView(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	want := []LocalOverride{{Module: "saas", Path: checkout}, {Module: "billing", Service: "api", Path: filepath.Join(checkout, "services", "gateway")}}
	if len(ignored) != len(want) || ignored[0] != want[0] || ignored[1] != want[1] {
		t.Fatalf("ignored = %+v, want %+v", ignored, want)
	}
	if got := ignored[0].Unit() + " " + ignored[1].Unit(); got != "saas billing/api" {
		t.Fatalf("units = %q", got)
	}

	viewed, err := resources.LoadWorkspaceFromDir(resources.WithLocalOverlayFilter(ctx, filter), dir)
	if err != nil {
		t.Fatal(err)
	}
	saas, err := viewed.ResolveModule(ctx, viewed.Modules[0])
	if err != nil {
		t.Fatal(err)
	}
	if saas.Dir == checkout || !strings.HasPrefix(saas.Dir, os.Getenv(ModuleCacheEnv)) || len(saas.Services) != 0 {
		t.Fatalf("saas must resolve to its pinned clone with no service override, got %+v", saas)
	}
	billingView, err := viewed.ResolveModule(ctx, viewed.Modules[1])
	if err != nil {
		t.Fatal(err)
	}
	if billingView.Dir != pinnedBilling || len(billingView.Services) != 0 {
		t.Fatalf("billing must keep the CLI's path and drop the service override, got %+v", billingView)
	}
	if _, err := viewed.LoadModuleFromName(ctx, "saas"); err != nil {
		t.Fatalf("the pinned view must be loadable: %v", err)
	}

	// The file still says what the developer wrote, and a plain load honours it.
	after, err := os.ReadFile(filepath.Join(dir, resources.LocalOverlayConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(written) {
		t.Fatalf("the overlay was rewritten:\n%s", after)
	}
	plain, err := workspace.ResolveModule(ctx, workspace.Modules[0])
	if err != nil || plain.Dir != checkout {
		t.Fatalf("a local load must still honour the override: %v %+v", err, plain)
	}
	if overrides, err := LocalOverrides(ctx, workspace); err != nil || len(overrides) != 2 {
		t.Fatalf("LocalOverrides = %+v, %v", overrides, err)
	}
}

// A module composed without a source is relocated by a path directive too;
// the view drops that entry so the module falls back to its committed layout.
func TestPinnedViewDropsAnInRepoModuleOverride(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	ctx := context.Background()
	dir := t.TempDir()
	for _, module := range []string{filepath.Join("modules", "web"), "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(dir, module), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, module, resources.ModuleConfigurationName), []byte("name: web\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte("name: solution\nlayout: modules\nmodules:\n    - name: web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, resources.LocalOverlayConfigurationName), []byte("resolve:\n    web:\n        path: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	filter, ignored, err := PinnedView(ctx, workspace)
	if err != nil || len(ignored) != 1 || ignored[0].Path != "elsewhere" {
		t.Fatalf("ignored = %+v, %v", ignored, err)
	}
	viewed, err := resources.LoadWorkspaceFromDir(resources.WithLocalOverlayFilter(ctx, filter), dir)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := viewed.ResolveModule(ctx, viewed.Modules[0])
	if err != nil || resolution.Dir != filepath.Join(dir, "modules", "web") {
		t.Fatalf("resolution = %+v, %v; want the committed layout", resolution, err)
	}
}
