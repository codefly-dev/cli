package generate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSolveOutputDirectoryCreatesNewDestination(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "generated", "users_api")
	resolved, err := solveOutputDirectory(context.Background(), destination)
	if err != nil {
		t.Fatalf("solveOutputDirectory: %v", err)
	}
	if resolved != destination {
		t.Fatalf("resolved destination = %q, want %q", resolved, destination)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("stat created destination: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("created destination is not a directory")
	}
}

func TestSolveOutputDirectoryRejectsFileAndEmptyPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := solveOutputDirectory(context.Background(), file); err == nil {
		t.Fatal("existing file was accepted as a generation directory")
	}
	if _, err := solveOutputDirectory(context.Background(), "   "); err == nil {
		t.Fatal("empty destination was accepted")
	}
}
