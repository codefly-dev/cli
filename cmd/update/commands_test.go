package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestUpdateCommandsReturnErrorsThroughCobra(t *testing.T) {
	for _, command := range []*cobra.Command{WorkspaceCmd, ServiceCmd, DepsCmd} {
		if command.RunE == nil || command.Run != nil {
			t.Errorf("%s is not exclusively RunE", command.Name())
		}
		if err := command.Args(command, []string{"extra"}); err == nil {
			t.Errorf("%s accepted a positional argument", command.Name())
		}
	}
}

func TestDepsCommandReturnsNoModuleError(t *testing.T) {
	previous := depsDir
	depsDir = t.TempDir()
	t.Cleanup(func() { depsDir = previous })
	if err := DepsCmd.RunE(DepsCmd, nil); err == nil {
		t.Fatal("dependency update without go.mod returned success")
	}
}

func TestDependencyWalksPropagateErrorsAndSkipFixtures(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := findGoModDirs(missing); err == nil {
		t.Fatal("missing module root returned no error")
	}
	if _, err := findGoWorkFiles(missing); err == nil {
		t.Fatal("missing workspace root returned no error")
	}

	root := t.TempDir()
	for _, file := range []string{
		filepath.Join(root, "go.mod"),
		filepath.Join(root, "go.work"),
		filepath.Join(root, "nested", "go.mod"),
		filepath.Join(root, "nested", "go.work"),
		filepath.Join(root, "testdata", "fixture", "go.mod"),
		filepath.Join(root, "testdata", "fixture", "go.work"),
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("module example.com/test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	modules, err := findGoModDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	workspaces, err := findGoWorkFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 2 {
		t.Fatalf("modules = %v, want root and nested only", modules)
	}
	if len(workspaces) != 2 {
		t.Fatalf("workspaces = %v, want root and nested only", workspaces)
	}
}

func TestDependencyRunGoPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runGo(ctx, t.TempDir(), "version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runGo error = %v, want context cancellation", err)
	}
}
