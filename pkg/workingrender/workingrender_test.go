package workingrender

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEveryEnvironmentHasAPathOfItsOwn(t *testing.T) {
	root := t.TempDir()
	if Module(root, "blue", "saas") == Module(root, "staging", "saas") {
		t.Fatal("two environments share one working render")
	}
	if want := filepath.Join(root, ".codefly", "renders", "blue", "modules", "saas"); Module(root, "blue", "saas") != want {
		t.Fatalf("Module = %s, want %s", Module(root, "blue", "saas"), want)
	}
}

func TestPrepareIgnoresTheRendersAndRefusesAnUnnamedEnvironment(t *testing.T) {
	root := t.TempDir()
	for _, environment := range []string{"", ".", "..", "a/b"} {
		if err := Prepare(root, environment); err == nil {
			t.Fatalf("Prepare(%q) scoped a render to no single environment", environment)
		}
	}
	for range 2 {
		if err := Prepare(root, "blue"); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(Modules(root, "blue")); err != nil || !info.IsDir() {
		t.Fatalf("the environment's modules directory was not created: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(Root(root), ".gitignore"))
	if err != nil || string(data) != "*\n" {
		t.Fatalf(".gitignore = %q, %v; want the root to ignore itself", data, err)
	}
}

func TestOnlyAModulesWorkingRenderHasARecord(t *testing.T) {
	root := t.TempDir()
	workspace, environment, module, ok := RecordOf(Module(root, "blue", "saas"))
	if !ok || workspace != root || environment != "blue" || module != "saas" {
		t.Fatalf("RecordOf(the working render of saas for blue) = %q %q %q %v", workspace, environment, module, ok)
	}
	if Record(root, "blue", "saas") == Record(root, "staging", "saas") {
		t.Fatal("two environments share one render record")
	}
	for _, other := range []string{
		filepath.Join(Environment(root, "blue"), "services", "saas", "api"),
		filepath.Join(root, "deployments", "modules", "saas"),
		filepath.Join(root, "renders", "blue", "modules", "saas"),
	} {
		if _, _, _, ok := RecordOf(other); ok {
			t.Fatalf("RecordOf(%s) answered a module's working render", other)
		}
	}
}
