package generate

import (
	"os"
	"path/filepath"
	"testing"
)

// buf names the source set: each file is read from the path buf found it at
// and named by its import path, and a file buf lists only as an import is not
// a source.
func TestProtoSourcesFromListingNamesByImportPath(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"acme/v1/api.proto", "vendor/upstream/base/v0/base.proto"} {
		writeTestFile(t, filepath.Join(root, rel), "syntax = \"proto3\";\n// "+rel+"\n")
	}
	listing := []byte(`{"path":"acme/v1/api.proto","import_path":"acme/v1/api.proto","is_import":false}
{"path":"vendor/upstream/base/v0/base.proto","import_path":"upstream/base/v0/base.proto","is_import":false}
{"path":"buf/validate/validate.proto","import_path":"buf/validate/validate.proto","is_import":true}
`)
	sources, err := protoSourcesFromListing(root, listing)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].Path != "acme/v1/api.proto" || sources[1].Path != "upstream/base/v0/base.proto" {
		t.Fatalf("sources = %+v", sources)
	}
	if string(sources[1].Content) != "syntax = \"proto3\";\n// vendor/upstream/base/v0/base.proto\n" {
		t.Fatalf("content read from the wrong file: %q", sources[1].Content)
	}
}

// A listed path that leaves the copied tree, or a listing with nothing to
// compile, is refused rather than read.
func TestProtoSourcesFromListingRefusesWhatItCannotRead(t *testing.T) {
	root := t.TempDir()
	if _, err := protoSourcesFromListing(root, []byte(`{"path":"../escape.proto","import_path":"escape.proto"}`)); err == nil {
		t.Fatal("a path outside the tree was read")
	}
	if _, err := protoSourcesFromListing(root, nil); err == nil {
		t.Fatal("an empty listing was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "escape.proto")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
