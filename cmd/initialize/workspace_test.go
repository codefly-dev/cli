package initialize

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
)

func TestWorkspaceCommandReturnsErrorsThroughCobra(t *testing.T) {
	if WorkspaceCmd.RunE == nil || WorkspaceCmd.Run != nil {
		t.Fatal("workspace command is not using RunE exclusively")
	}
	if err := WorkspaceCmd.Args(WorkspaceCmd, nil); err == nil {
		t.Fatal("missing workspace name was accepted")
	}
	if err := WorkspaceCmd.Args(WorkspaceCmd, []string{"workspace"}); err != nil {
		t.Fatalf("valid workspace name rejected: %v", err)
	}
}

func TestNewWorkspaceRejectsTraversalWithoutLeavingDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	escapedName := "escaped-" + filepath.Base(root)
	escaped := filepath.Join(filepath.Dir(root), escapedName)

	previousDefault, previousLayout := cli.WithDefault(), layout
	cli.SetWithDefault(true)
	layout = resources.LayoutKindModules
	t.Cleanup(func() {
		cli.SetWithDefault(previousDefault)
		layout = previousLayout
	})

	if err := newWorkspace("../" + escapedName); err == nil {
		t.Fatal("traversing workspace name returned success")
	}
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("escaped workspace directory exists: %v", err)
	}
}
