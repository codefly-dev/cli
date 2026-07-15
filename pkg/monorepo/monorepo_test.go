package monorepo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindRootUsesCoreModuleDeclaration(t *testing.T) {
	root := t.TempDir()
	core := filepath.Join(root, "core")
	nested := filepath.Join(root, "agents", "services", "postgres")
	if err := os.MkdirAll(core, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(core, "go.mod"), []byte("module "+CoreModulePath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := FindRoot(nested); got != root {
		t.Fatalf("FindRoot(%q) = %q, want %q", nested, got, root)
	}
}

func TestFindRootRejectsLookalikeDirectory(t *testing.T) {
	root := t.TempDir()
	core := filepath.Join(root, "core")
	if err := os.MkdirAll(core, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(core, "go.mod"), []byte("module example.com/core\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := FindRoot(root); got != "" {
		t.Fatalf("FindRoot(%q) = %q, want no match", root, got)
	}
}

func TestIsModuleAcceptsWhitespaceAndComments(t *testing.T) {
	dir := t.TempDir()
	contents := "// generated\n\nmodule\t" + CLIModulePath + " // CLI source\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsModule(dir, CLIModulePath) {
		t.Fatal("IsModule rejected a valid module directive")
	}
}
