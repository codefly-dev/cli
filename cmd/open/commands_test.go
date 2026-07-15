package open

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestOpenCommandsReturnErrorsThroughCobra(t *testing.T) {
	for _, command := range []*cobra.Command{WorkspaceCmd, ModuleCmd, ServiceCmd} {
		if command.RunE == nil || command.Run != nil {
			t.Errorf("%s is not exclusively RunE", command.Name())
		}
		if err := command.Args(command, []string{"extra"}); err == nil {
			t.Errorf("%s accepted a positional argument", command.Name())
		}
	}
}
