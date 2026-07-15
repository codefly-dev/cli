package show

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestShowCommandsReturnErrorsThroughCobra(t *testing.T) {
	for _, command := range []*cobra.Command{DependenciesCmd, NetworkCmd} {
		if command.RunE == nil || command.Run != nil {
			t.Errorf("%s is not exclusively RunE", command.Name())
		}
	}
	if err := DependenciesCmd.Args(DependenciesCmd, []string{"one", "two"}); err == nil {
		t.Fatal("dependencies accepted two service names")
	}
	if err := NetworkCmd.Args(NetworkCmd, []string{"extra"}); err == nil {
		t.Fatal("network accepted a positional argument")
	}
}

func TestNetworkMissingWorkspaceReturnsErrorWithoutServiceSelection(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := NetworkCmd.RunE(NetworkCmd, nil); err == nil {
		t.Fatal("network returned success without a workspace")
	}
}
