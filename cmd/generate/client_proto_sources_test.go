package generate

import (
	"os"
	"path/filepath"
	"strings"
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

// A linked folder outside the tree is copied as what it holds; a link to a
// folder that holds it, or to one already being copied, is refused by name
// rather than recursed into.
func TestCopyTreeFollowsLinkedFoldersAndRefusesLoops(t *testing.T) {
	root := t.TempDir()
	src, shared := filepath.Join(root, "proto"), filepath.Join(root, "shared")
	writeTestFile(t, filepath.Join(src, "acme/v1/api.proto"), "a")
	writeTestFile(t, filepath.Join(shared, "upstream/base.proto"), "b")
	if err := os.Symlink(shared, filepath.Join(src, "vendor")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "vendor/upstream/base.proto")); err != nil || string(data) != "b" {
		t.Fatalf("linked folder not copied: %q, %v", data, err)
	}

	for name, link := range map[string]struct{ at, to string }{
		"to its own folder":      {"acme/loop", "acme"},
		"to the copied root":     {"acme/v1/up", "."},
		"back through a link":    {"../shared/upstream/back", "."},
		"to a folder above root": {"acme/out", ".."},
	} {
		t.Run(name, func(t *testing.T) {
			at := filepath.Join(src, filepath.FromSlash(link.at))
			if err := os.Symlink(filepath.Join(src, link.to), at); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(at) })
			err := copyTree(src, filepath.Join(t.TempDir(), "copy"))
			if err == nil || !strings.Contains(err.Error(), "looping link") {
				t.Fatalf("copyTree = %v, want a looping link refused", err)
			}
			resolved, _ := filepath.EvalSymlinks(at)
			if !strings.Contains(err.Error(), resolved) {
				t.Fatalf("refusal %q does not name the link's target %s", err, resolved)
			}
		})
	}
}
