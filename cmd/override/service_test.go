package override

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func writeOverrideFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// overrideWorkspace lays out a solution composing one module with two services,
// chdirs into it, and resets the command's flag state around the test.
func overrideWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "solution",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "saas", Source: "acme/host", Version: "1.0"}},
	}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	moduleDir := filepath.Join(root, "modules", "saas")
	writeOverrideFile(t, filepath.Join(moduleDir, "module.codefly.yaml"),
		"kind: module\nname: saas\nservices:\n  - name: gateway\n  - name: api\n")
	for _, service := range []string{"gateway", "api"} {
		writeOverrideFile(t, filepath.Join(moduleDir, "services", service, "service.codefly.yaml"),
			"kind: service\nname: "+service+"\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n")
	}
	t.Chdir(root)
	t.Cleanup(func() { servicePath, serviceWorktree, serviceVersion, serviceClear = "", "", "", false })
	return root
}

func loadOverride(t *testing.T, root, module, service string) *resources.ServiceResolveDirective {
	t.Helper()
	overlay, err := resources.LoadLocalOverlay(context.Background(), root)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	if overlay == nil || overlay.Resolve[module] == nil {
		return nil
	}
	return overlay.Resolve[module].Services[service]
}

// The whole point of the command: the override lands in the machine-local
// overlay and the committed workspace file is untouched.
func TestOverrideServiceWritesOverlayAndNeverCommittedConfig(t *testing.T) {
	root := overrideWorkspace(t)
	committed := filepath.Join(root, resources.WorkspaceConfigurationName)
	before, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	writeOverrideFile(t, filepath.Join(checkout, "service.codefly.yaml"),
		"kind: service\nname: gateway\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.9.9\n  publisher: codefly.ai\n")

	servicePath = checkout
	if err := runOverrideService(nil, []string{"saas/gateway"}); err != nil {
		t.Fatalf("override: %v", err)
	}

	override := loadOverride(t, root, "saas", "gateway")
	if override == nil || override.Path != checkout {
		t.Fatalf("overlay does not record the override: %+v", override)
	}
	after, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("committed config was modified:\n%s\n---\n%s", before, after)
	}
	if loadOverride(t, root, "saas", "api") != nil {
		t.Fatal("overriding one service touched another")
	}
}

// A relative --path is stored absolute: the overlay is read from wherever a run
// starts, so a path relative to the shell that wrote it would not survive.
func TestOverrideServiceStoresAnAbsolutePath(t *testing.T) {
	root := overrideWorkspace(t)
	writeOverrideFile(t, filepath.Join(root, "elsewhere", "service.codefly.yaml"),
		"kind: service\nname: gateway\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n")

	servicePath = "elsewhere"
	if err := runOverrideService(nil, []string{"saas/gateway"}); err != nil {
		t.Fatalf("override: %v", err)
	}
	override := loadOverride(t, root, "saas", "gateway")
	if !filepath.IsAbs(override.Path) {
		t.Fatalf("override path is not absolute: %s", override.Path)
	}
}

// The overlay records machine-local absolute paths, so the command that creates
// it must also keep it out of git — otherwise the next `git add -A` commits a
// path that is meaningless on every other machine.
func TestOverrideServiceGitignoresTheOverlayItCreates(t *testing.T) {
	root := overrideWorkspace(t)
	checkout := t.TempDir()
	writeOverrideFile(t, filepath.Join(checkout, "service.codefly.yaml"),
		"kind: service\nname: gateway\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n")

	servicePath = checkout
	if err := runOverrideService(nil, []string{"saas/gateway"}); err != nil {
		t.Fatalf("override: %v", err)
	}

	ignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("no .gitignore written beside the overlay: %v", err)
	}
	if !strings.Contains(string(ignore), resources.LocalOverlayConfigurationName) {
		t.Fatalf(".gitignore does not ignore the overlay: %q", ignore)
	}
}

// Clearing an override that is not there changed nothing, so it must not bring
// a codefly.local.yaml into existence.
func TestOverrideServiceClearWritesNothingWhenThereIsNoOverride(t *testing.T) {
	root := overrideWorkspace(t)
	serviceClear = true
	if err := runOverrideService(nil, []string{"saas/gateway"}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, resources.LocalOverlayConfigurationName)); !os.IsNotExist(err) {
		t.Fatalf("a no-op clear created an overlay file (stat err = %v)", err)
	}
}

// An override that does not resolve is not written: the overlay every later
// command reads is left exactly as it was found.
func TestOverrideServiceDoesNotLeaveAnUnresolvableOverrideBehind(t *testing.T) {
	root := overrideWorkspace(t)
	serviceWorktree = "acme/nowhere@no-such-ref"
	if err := runOverrideService(nil, []string{"saas/gateway"}); err == nil {
		t.Fatal("an unresolvable worktree override was accepted")
	}
	if override := loadOverride(t, root, "saas", "gateway"); override != nil {
		t.Fatalf("a rejected override was left in the overlay: %+v", override)
	}
}

// --clear removes the override, and the module entry with it when nothing else
// selected the module: core rejects an entry that selects nothing at all.
func TestOverrideServiceClearRemovesTheEntry(t *testing.T) {
	root := overrideWorkspace(t)
	serviceVersion = "0.0.66"
	if err := runOverrideService(nil, []string{"saas/gateway"}); err != nil {
		t.Fatalf("override: %v", err)
	}
	if loadOverride(t, root, "saas", "gateway") == nil {
		t.Fatal("override was not written")
	}

	serviceVersion, serviceClear = "", true
	if err := runOverrideService(nil, []string{"saas/gateway"}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if loadOverride(t, root, "saas", "gateway") != nil {
		t.Fatal("override survived --clear")
	}
	overlay, err := resources.LoadLocalOverlay(context.Background(), root)
	if err != nil {
		t.Fatalf("overlay is unreadable after --clear: %v", err)
	}
	if overlay.Resolve["saas"] != nil {
		t.Fatalf("an entry selecting nothing was left behind: %+v", overlay.Resolve["saas"])
	}
}

func TestOverrideServiceRejectsAnUncomposedModule(t *testing.T) {
	overrideWorkspace(t)
	servicePath = t.TempDir()
	err := runOverrideService(nil, []string{"billing/ledger"})
	if err == nil || !strings.Contains(err.Error(), "billing") || !strings.Contains(err.Error(), "saas") {
		t.Fatalf("error does not name the module asked for and the ones composed: %v", err)
	}
}

func TestOverrideServiceRequiresAModuleQualifiedCoordinate(t *testing.T) {
	overrideWorkspace(t)
	servicePath = t.TempDir()
	if err := runOverrideService(nil, []string{"gateway"}); err == nil {
		t.Fatal("a bare service name was accepted")
	}
}

func TestSelectedDirectiveRequiresExactlyOneSource(t *testing.T) {
	t.Cleanup(func() { servicePath, serviceWorktree, serviceVersion, serviceClear = "", "", "", false })

	if _, err := selectedDirective(); err == nil {
		t.Fatal("no source selected was accepted")
	}
	servicePath, serviceVersion = t.TempDir(), "0.0.1"
	if _, err := selectedDirective(); err == nil {
		t.Fatal("two sources selected were accepted")
	}
	servicePath, serviceVersion = "", "0.0.1"
	directive, err := selectedDirective()
	if err != nil {
		t.Fatalf("one source rejected: %v", err)
	}
	if directive.Version != "0.0.1" {
		t.Fatalf("directive does not carry the version: %+v", directive)
	}
	serviceVersion, serviceWorktree = "", "no-at-sign"
	if _, err := selectedDirective(); err == nil {
		t.Fatal("a malformed worktree coordinate was accepted")
	}
}
