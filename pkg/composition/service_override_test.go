package composition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// initServiceModuleRepo builds a module repo whose module carries one service,
// tagged at each of versions. A per-service override pulls the module package
// and takes the service out of it, so the fixture has to be a real module tree
// rather than a bare manifest.
func initServiceModuleRepo(t *testing.T, service string, versions ...string) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "config", "user.email", "pinned@example.invalid")
	runGit(t, repo, "config", "user.name", "Pinned Test")
	moduleDir := filepath.Join(repo, "module")
	serviceDir := filepath.Join(moduleDir, "services", service)
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, resources.ModuleConfigurationName),
		[]byte("name: saas\nservices:\n  - name: "+service+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serviceDir, resources.ServiceConfigurationName),
		[]byte("kind: service\nname: "+service+"\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "module")
	for _, version := range versions {
		runGit(t, repo, "-c", "tag.gpgSign=false", "tag", version)
	}
	return "file://" + repo
}

func serviceOverrideWorkspace(t *testing.T, dir, source, moduleVersion, overlay string) *resources.Workspace {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, resources.LocalOverlayConfigurationName), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace := &resources.Workspace{
		Name:    "solution",
		Modules: []*resources.ModuleReference{{Name: "saas", Source: source, Module: "module", Version: moduleVersion}},
	}
	workspace.WithDir(dir)
	return workspace
}

// A `version:` service override is materialized exactly as a pinned module is:
// the package is pulled, the directive is replaced by the service directory
// inside it, and a receipt keyed <module>/<service> records the request.
func TestMaterializeServiceOverrideVersionRewritesToPathAndReceipt(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initServiceModuleRepo(t, "gateway", "v0.0.1", "v0.0.2")
	dir := t.TempDir()
	workspace := serviceOverrideWorkspace(t, dir, source, "v0.0.1",
		"resolve:\n  saas:\n    git: true\n    services:\n      gateway:\n        version: v0.0.2\n")

	if err := MaterializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	overlay, err := resources.LoadLocalOverlay(context.Background(), dir)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	entry := overlay.Resolve["saas"]
	if entry.Path == "" {
		t.Fatalf("module entry lost its materialized path: %+v", entry)
	}
	override := entry.Services["gateway"]
	if override == nil || override.Version != "" || override.Path == "" {
		t.Fatalf("service override was not rewritten to a path: %+v", override)
	}
	if filepath.Base(override.Path) != "gateway" || filepath.Base(filepath.Dir(override.Path)) != "services" {
		t.Fatalf("service override path is not <module>/services/gateway: %s", override.Path)
	}
	if _, err := os.Stat(filepath.Join(override.Path, resources.ServiceConfigurationName)); err != nil {
		t.Fatalf("materialized service dir has no manifest: %v", err)
	}
	// The module and the service were pulled at different versions, so the
	// service must NOT have been taken out of the module's own checkout.
	if override.Path == filepath.Join(entry.Path, "services", "gateway") {
		t.Fatalf("service override resolved inside the module's own v0.0.1 checkout: %s", override.Path)
	}

	receipts, err := LoadResolutionReceipts(dir)
	if err != nil {
		t.Fatalf("load receipts: %v", err)
	}
	receipt := receipts["saas/gateway"]
	if receipt == nil {
		t.Fatalf("no receipt keyed saas/gateway: %+v", receipts)
	}
	if receipt.Service != "gateway" || receipt.Requested != "v0.0.2" || receipt.Path != override.Path {
		t.Fatalf("receipt does not record the request it answered: %+v", receipt)
	}
	if receipts["saas"] == nil || receipts["saas"].Service != "" {
		t.Fatalf("module receipt was disturbed: %+v", receipts["saas"])
	}
}

// A changed request that cannot be resolved must fail closed rather than let
// the previous materialization answer it: that one answered the OLD request,
// and running it is how a failed bump keeps silently shipping the version
// before it. The user's own `version:` edit is left in the overlay — it is their
// request, not machine output, and it selects no directory to run from.
func TestMaterializeServiceOverrideStaleRequestFailsNamingBothVersions(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initServiceModuleRepo(t, "gateway", "v0.0.1", "v0.0.2")
	dir := t.TempDir()
	workspace := serviceOverrideWorkspace(t, dir, source, "v0.0.1",
		"resolve:\n  saas:\n    git: true\n    services:\n      gateway:\n        version: v0.0.2\n")

	if err := MaterializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	overlay, _ := resources.LoadLocalOverlay(context.Background(), dir)
	materialized := overlay.Resolve["saas"].Services["gateway"].Path

	// Ask for a version the source does not carry.
	overlay.Resolve["saas"].Services["gateway"] = &resources.ServiceResolveDirective{Version: "v9.9.9"}
	if err := resources.SaveLocalOverlay(context.Background(), dir, overlay); err != nil {
		t.Fatal(err)
	}

	err := MaterializePinnedModules(context.Background(), workspace)
	if err == nil {
		t.Fatal("materialize accepted an unresolvable service version")
	}
	for _, want := range []string{"gateway", "v9.9.9", "v0.0.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not name %q: %v", want, err)
		}
	}

	reloaded, _ := resources.LoadLocalOverlay(context.Background(), dir)
	override := reloaded.Resolve["saas"].Services["gateway"]
	if override.Version != "v9.9.9" {
		t.Fatalf("the user's own request was rewritten: %+v", override)
	}
	if override.Path != "" {
		t.Fatalf("a stale path is still selected for the service: %s", override.Path)
	}
	// The receipt is kept: outside a cache root it is the only thing that
	// identifies that path as machine output rather than a user checkout.
	receipts, _ := LoadResolutionReceipts(dir)
	if receipts["saas/gateway"].ResolvedPath() != materialized {
		t.Fatalf("ownership receipt was dropped: %+v", receipts["saas/gateway"])
	}
}

// The same rule reached through the other door: when the overlay still selects
// the path the CLI wrote and the request recorded behind it can no longer be
// resolved — a tag deleted at the source, say — that path IS dropped, so
// nothing stale is left selected.
func TestMaterializeServiceOverrideDropsItsOwnPathWhenTheRequestStopsResolving(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initServiceModuleRepo(t, "gateway", "v0.0.1", "v0.0.2")
	dir := t.TempDir()
	workspace := serviceOverrideWorkspace(t, dir, source, "v0.0.1",
		"resolve:\n  saas:\n    git: true\n    services:\n      gateway:\n        version: v0.0.2\n")

	if err := MaterializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	overlay, _ := resources.LoadLocalOverlay(context.Background(), dir)
	if overlay.Resolve["saas"].Services["gateway"].Path == "" {
		t.Fatal("service override was not materialized to a path")
	}

	// The overlay keeps selecting that path, but the version it was pulled for
	// is no longer available at the source.
	receipts, _ := LoadResolutionReceipts(dir)
	receipts["saas/gateway"].Requested = "v9.9.9"
	if err := SaveResolutionReceipts(context.Background(), dir, receipts); err != nil {
		t.Fatal(err)
	}

	err := MaterializePinnedModules(context.Background(), workspace)
	if err == nil {
		t.Fatal("materialize accepted a service path whose request no longer resolves")
	}
	if !strings.Contains(err.Error(), "gateway") || !strings.Contains(err.Error(), resources.LocalOverlayConfigurationName) {
		t.Fatalf("error does not name the service and the file it dropped from: %v", err)
	}

	reloaded, _ := resources.LoadLocalOverlay(context.Background(), dir)
	if entry := reloaded.Resolve["saas"]; entry != nil && entry.Services["gateway"] != nil {
		t.Fatalf("stale service path survived a failed re-resolution: %+v", entry.Services["gateway"])
	}
	// The module itself is untouched: only the service override went stale.
	if reloaded.Resolve["saas"] == nil || reloaded.Resolve["saas"].Path == "" {
		t.Fatalf("the module's own materialization was dropped with the service: %+v", reloaded.Resolve["saas"])
	}
}

// A module that fails to re-resolve must not take the user's service overrides
// with it: those are independent intent about individual services, and the
// module's failure says nothing about them.
func TestMaterializeKeepsServiceOverridesWhenTheModuleFailsToResolve(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initServiceModuleRepo(t, "gateway", "v0.0.1")
	dir := t.TempDir()
	elsewhere := t.TempDir()
	workspace := serviceOverrideWorkspace(t, dir, source, "v0.0.1",
		"resolve:\n  saas:\n    git: true\n    services:\n      gateway:\n        path: "+elsewhere+"\n")

	if err := MaterializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	workspace.Modules[0].Source = "file://" + filepath.Join(t.TempDir(), "gone")
	if err := MaterializePinnedModules(context.Background(), workspace); err == nil {
		t.Fatal("materialize accepted an unresolvable module")
	}

	reloaded, _ := resources.LoadLocalOverlay(context.Background(), dir)
	entry := reloaded.Resolve["saas"]
	if entry == nil || entry.Services["gateway"] == nil || entry.Services["gateway"].Path != elsewhere {
		t.Fatalf("the user's service override was dropped with the module: %+v", entry)
	}
}

// Rewriting the module entry to its materialized path must carry the service
// overrides across: they select services, not the module, and losing them would
// silently put every overridden service back on the module's own copy.
func TestMaterializeKeepsServiceOverridesWhenRewritingTheModuleEntry(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := initServiceModuleRepo(t, "gateway", "v0.0.1")
	dir := t.TempDir()
	elsewhere := t.TempDir()
	workspace := serviceOverrideWorkspace(t, dir, source, "v0.0.1",
		"resolve:\n  saas:\n    git: true\n    services:\n      gateway:\n        path: "+elsewhere+"\n")

	if err := MaterializePinnedModules(context.Background(), workspace); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	overlay, _ := resources.LoadLocalOverlay(context.Background(), dir)
	entry := overlay.Resolve["saas"]
	if entry.Path == "" || entry.Git {
		t.Fatalf("module entry was not rewritten to its clone path: %+v", entry)
	}
	if entry.Services["gateway"] == nil || entry.Services["gateway"].Path != elsewhere {
		t.Fatalf("the user's service override was lost in the rewrite: %+v", entry.Services)
	}
	// A path the user wrote is machine-local intent, never refreshed and never
	// recorded as machine output.
	receipts, _ := LoadResolutionReceipts(dir)
	if receipts["saas/gateway"] != nil {
		t.Fatalf("a user-written service path was claimed as machine output: %+v", receipts["saas/gateway"])
	}
}

func TestServiceManaged(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	cases := map[string]struct {
		directive *resources.ServiceResolveDirective
		recorded  string
		managed   bool
	}{
		"version names a strategy":  {&resources.ServiceResolveDirective{Version: "0.0.1"}, "", true},
		"worktree is the user's":    {&resources.ServiceResolveDirective{Worktree: "acme/host@main"}, "", false},
		"user path is left alone":   {&resources.ServiceResolveDirective{Path: "/home/me/svc"}, "", false},
		"path on the receipt":       {&resources.ServiceResolveDirective{Path: "/moved/svc"}, "/moved/svc", true},
		"path under a cache root":   {&resources.ServiceResolveDirective{Path: filepath.Join(cacheRoot, "x", "services", "y")}, "", true},
		"empty selects nothing yet": {&resources.ServiceResolveDirective{}, "", false},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := serviceManaged(testCase.directive, testCase.recorded, cacheRoot); got != testCase.managed {
				t.Fatalf("serviceManaged = %v, want %v", got, testCase.managed)
			}
		})
	}
}

// A receipt for an override the overlay still asks for must survive pruning;
// one for an override the user has removed must not.
func TestPruneStaleReceiptsKeepsRequestedServiceOverrides(t *testing.T) {
	receipts := map[string]*ResolutionReceipt{
		"saas":           {Path: "/cache/saas"},
		"saas/gateway":   {Service: "gateway", Path: "/cache/saas/services/gateway"},
		"saas/telemetry": {Service: "telemetry", Path: "/cache/saas/services/telemetry"},
		"gone":           {Path: "/cache/gone"},
	}
	modules := []*resources.ModuleReference{{Name: "saas"}}
	requested := map[string]bool{"saas/gateway": true}

	if !pruneStaleReceipts(receipts, modules, requested) {
		t.Fatal("pruneStaleReceipts reported no change")
	}
	if _, ok := receipts["saas/gateway"]; !ok {
		t.Fatal("a still-requested service override receipt was pruned")
	}
	if _, ok := receipts["saas/telemetry"]; ok {
		t.Fatal("a removed service override receipt survived")
	}
	if _, ok := receipts["gone"]; ok {
		t.Fatal("a removed module's receipt survived")
	}
}
