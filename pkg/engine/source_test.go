package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
)

// TestNewSourceExecutesLanguageNeutralOperation locks the newSource contract:
// whichever build variant is compiled (analyzer via source_semantic.go, or the
// CGO-free source_nosemantic.go selected by -tags codefly_nosemantic), the
// constructor must return a live Source whose base, language-neutral behavior
// works. Run under `-tags codefly_nosemantic` this is the only cli-side test
// that exercises the analyzer-free variant's runtime, closing the gap where the
// static companion build was verified to link but never to function.
func TestNewSourceExecutesLanguageNeutralOperation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed source tree: %v", err)
	}

	source := newSource(root)
	if source == nil {
		t.Fatal("newSource returned nil")
	}
	t.Cleanup(func() { _ = source.Close() })

	response, err := source.ExecuteCode(context.Background(), &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ListFiles{ListFiles: &codev0.ListFilesRequest{}},
	})
	if err != nil {
		t.Fatalf("ListFiles execute: %v", err)
	}
	if failure := response.GetFailure(); failure != nil {
		t.Fatalf("ListFiles reported failure: %v", failure)
	}

	found := false
	for _, file := range response.GetListFiles().GetFiles() {
		if filepath.Base(file.GetPath()) == "marker.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ListFiles did not return the seeded file; got %+v", response.GetListFiles().GetFiles())
	}
}
