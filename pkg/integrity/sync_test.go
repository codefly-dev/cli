package integrity

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBaseSyncPreservesOverlaysAndAppliesOnlyOwnedFiles(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "stable.txt"), "stable")
	writeTestFile(t, filepath.Join(source, "update.txt"), "new")
	writeTestFile(t, filepath.Join(source, "create.txt"), "created")
	writeTestFile(t, filepath.Join(source, "module.codefly.yaml"), "canonical descriptor")
	writeTestFile(t, filepath.Join(target, "stable.txt"), "stable")
	writeTestFile(t, filepath.Join(target, "update.txt"), "old")
	writeTestFile(t, filepath.Join(target, "remove.txt"), "retired")
	writeTestFile(t, filepath.Join(target, "module.codefly.yaml"), targetModuleYAML)
	writeTestFile(t, filepath.Join(target, "product", "plugin.ts"), "warden overlay")
	writeTestJSON(t, filepath.Join(target, "tools", "base-integrity-allow.json"), map[string]any{
		"module.codefly.yaml": "consumer composition root",
		"requiredAdditions":   map[string]string{"product/plugin.ts": "product frontend plugin"},
	})
	writeManifest(t, source, "stable.txt", "update.txt", "create.txt", "module.codefly.yaml")
	writeManifest(t, target, "stable.txt", "update.txt", "remove.txt", "module.codefly.yaml")

	plan, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Applicable(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Create, []string{"create.txt"}) ||
		!reflect.DeepEqual(plan.Update, []string{"update.txt"}) ||
		!reflect.DeepEqual(plan.Remove, []string{"remove.txt"}) ||
		!reflect.DeepEqual(plan.Allowed, []string{"module.codefly.yaml"}) {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if _, err := ApplyBaseSync(source, target); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(target, "update.txt"), "new")
	assertFileContents(t, filepath.Join(target, "create.txt"), "created")
	assertFileContents(t, filepath.Join(target, "product", "plugin.ts"), "warden overlay")
	assertFileContents(t, filepath.Join(target, "module.codefly.yaml"), targetModuleYAML)
	if _, err := os.Stat(filepath.Join(target, "remove.txt")); !os.IsNotExist(err) {
		t.Fatalf("retired base file still exists: %v", err)
	}
}

func TestBaseSyncRefusesModifiedBaseAndOverlayCollisionWithoutMutation(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "owned.txt"), "new owned")
	writeTestFile(t, filepath.Join(source, "collision.txt"), "new base")
	writeTestFile(t, filepath.Join(target, "owned.txt"), "product edited base")
	writeTestFile(t, filepath.Join(target, "collision.txt"), "product side addition")
	writeManifest(t, source, "owned.txt", "collision.txt")
	writeTestJSON(t, filepath.Join(target, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"owned.txt": digestOf(t, "old owned")},
	})

	plan, err := ApplyBaseSync(source, target)
	if err == nil {
		t.Fatal("unsafe base sync unexpectedly applied")
	}
	if !reflect.DeepEqual(plan.Modified, []string{"owned.txt"}) || !reflect.DeepEqual(plan.Collisions, []string{"collision.txt"}) {
		t.Fatalf("unexpected refusal plan: %#v", plan)
	}
	assertFileContents(t, filepath.Join(target, "owned.txt"), "product edited base")
	assertFileContents(t, filepath.Join(target, "collision.txt"), "product side addition")
}

func TestBaseSyncDoesNotInstallServicesOutsideConsumerComposition(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "services", "kept", "base.txt"), "new kept")
	writeTestFile(t, filepath.Join(source, "services", "omitted", "base.txt"), "do not install")
	writeTestFile(t, filepath.Join(target, "services", "kept", "base.txt"), "old kept")
	writeManifest(t, source, "services/kept/base.txt", "services/omitted/base.txt")
	writeManifest(t, target, "services/kept/base.txt")

	plan, err := ApplyBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Omitted, []string{"services/omitted/base.txt"}) {
		t.Fatalf("omitted = %v", plan.Omitted)
	}
	assertFileContents(t, filepath.Join(target, "services", "kept", "base.txt"), "new kept")
	if _, err := os.Stat(filepath.Join(target, "services", "omitted", "base.txt")); !os.IsNotExist(err) {
		t.Fatalf("omitted service was installed: %v", err)
	}
}

func TestBaseSyncRejectsManifestTraversalBeforeMutation(t *testing.T) {
	source, target := syncFixture(t)
	writeTestJSON(t, filepath.Join(source, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"../outside.txt": digestOf(t, "outside")},
	})
	writeManifest(t, target)

	plan, err := ApplyBaseSync(source, target)
	if err == nil {
		t.Fatal("manifest traversal unexpectedly applied")
	}
	if !reflect.DeepEqual(plan.SourceInvalid, []string{"../outside.txt"}) {
		t.Fatalf("source invalid = %v", plan.SourceInvalid)
	}
}

const targetModuleYAML = `kind: module
name: app
services:
  - name: kept
`

func syncFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	writeTestFile(t, filepath.Join(source, "module.codefly.yaml"), targetModuleYAML)
	writeTestFile(t, filepath.Join(target, "module.codefly.yaml"), targetModuleYAML)
	return source, target
}

func writeManifest(t *testing.T, root string, paths ...string) {
	t.Helper()
	manifest := baseManifest{Files: map[string]string{}}
	for _, relative := range paths {
		digest, err := sha256File(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		manifest.Files[relative] = digest
	}
	writeTestJSON(t, filepath.Join(root, "tools", "base-manifest.json"), manifest)
}

func digestOf(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "value")
	writeTestFile(t, path, contents)
	digest, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != expected {
		t.Fatalf("%s = %q, want %q", path, payload, expected)
	}
}
