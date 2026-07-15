package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
)

func TestDepsCommandReturnsValidationErrors(t *testing.T) {
	if DepsCmd.RunE == nil || DepsCmd.Run != nil {
		t.Fatal("agent deps must return errors through RunE")
	}
	if err := DepsCmd.Args(DepsCmd, []string{"extra"}); err == nil {
		t.Fatal("agent deps accepted positional arguments")
	}

	previousLink, _ := DepsCmd.Flags().GetBool("link")
	previousUnlink, _ := DepsCmd.Flags().GetBool("unlink")
	if err := DepsCmd.Flags().Set("link", "true"); err != nil {
		t.Fatal(err)
	}
	if err := DepsCmd.Flags().Set("unlink", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = DepsCmd.Flags().Set("link", boolString(previousLink))
		_ = DepsCmd.Flags().Set("unlink", boolString(previousUnlink))
	})
	if err := DepsCmd.RunE(DepsCmd, nil); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("link/unlink error = %v", err)
	}
}

func TestFindGoModDirsReturnsErrorsAndSkipsGeneratedTrees(t *testing.T) {
	if _, err := findGoModDirs(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing root did not return a walk error")
	}
	root := t.TempDir()
	want := []string{root, filepath.Join(root, "nested")}
	for _, dir := range append(want, filepath.Join(root, "testdata", "fixture")) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findGoModDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("module dirs = %v, want %v", got, want)
	}
}

func TestDiscoverAgentDirsWalksRecursivelyAndSkipsTestdata(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "agents", "services", "group", "agent")
	skipped := filepath.Join(root, "testdata", "fixture")
	for _, dir := range []string{deep, skipped} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), []byte("name: test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := discoverAgentDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{deep}) {
		t.Fatalf("agent dirs = %v, want %v", got, []string{deep})
	}
}

func TestFileSnapshotsRestoreExistingAndRemoveCreated(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "go.mod")
	created := filepath.Join(root, "go.sum")
	if err := os.WriteFile(existing, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotFiles(existing, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, done := common.NewContext()
	defer done()
	if err := restoreFiles(ctx, snapshots); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "original" {
		t.Fatalf("restored contents = %q", contents)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("new file was not removed: %v", err)
	}
}

func TestRunGoHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runGo(ctx, t.TempDir(), "version"); !errors.Is(err, context.Canceled) {
		t.Fatalf("runGo error = %v, want context cancellation", err)
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
