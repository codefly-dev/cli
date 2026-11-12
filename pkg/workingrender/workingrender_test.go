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
