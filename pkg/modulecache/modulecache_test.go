package modulecache

import (
	"path/filepath"
	"testing"
)

// Only a root that holds nothing but CLI output proves a path is a pinned
// release; a checkout beside it, or any path under a configured root, does not.
func TestOwnedIsOnlyTheCLIsOwnRoots(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv(Env, "")
	clone, err := CloneRoot()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		filepath.Join(clone, "acme", "module-x", "v1.2.3", "services", "api"):    true,
		filepath.Join(workspace, ".codefly", "cache", "modules", "sha256", "ab"): true,
		filepath.Join(workspace, "modules", "x", "services", "api"):              false,
		filepath.Join(filepath.Dir(clone), "modules-checkout", "x"):              false,
	} {
		if got, err := Owned(workspace, path); err != nil || got != want {
			t.Errorf("Owned(%s) = %v, %v, want %v", path, got, err, want)
		}
	}
	t.Setenv(Env, t.TempDir())
	configured, _ := Configured()
	if got, err := Owned(workspace, filepath.Join(configured, "acme", "module-x", "v1.2.3")); err != nil || got {
		t.Errorf("a configured root proved ownership: %v, %v", got, err)
	}
}
