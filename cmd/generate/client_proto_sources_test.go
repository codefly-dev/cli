package generate

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// A v2 buf.yaml with a vendor module and an excluded tree: every source is
// named relative to the module that holds it, as its importers spell it, and an
// excluded tree is not a source.
func TestCollectProtoSourcesFollowsBufModules(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("buf.yaml", "version: v2\nmodules:\n  - path: .\n    excludes:\n      - facade\n      - vendor\n  - path: vendor\n")
	write("acme/v1/api.proto", "syntax = \"proto3\";\n")
	write("facade/api.proto", "syntax = \"proto3\";\n")
	write("vendor/upstream/base/v0/base.proto", "syntax = \"proto3\";\n")

	sources, err := collectProtoSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, source := range sources {
		names = append(names, source.Path)
	}
	sort.Strings(names)
	want := []string{"acme/v1/api.proto", "upstream/base/v0/base.proto"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("sources = %v, want %v", names, want)
	}
}

// A buf.yaml declaring no modules is one module at its root.
func TestCollectProtoSourcesSingleModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "buf.yaml"), []byte("version: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acme", "api.proto"), []byte("syntax = \"proto3\";\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, err := collectProtoSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Path != "acme/api.proto" {
		t.Fatalf("sources = %+v", sources)
	}
}
