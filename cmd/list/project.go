package list

import (
	"errors"

	"github.com/spf13/cobra"
)

// WorkspaceCmd represents the run command
var WorkspaceCmd = &cobra.Command{
	Use:    "workspaces",
	Short:  "List all workspaces for the workspace",
	Hidden: true, // not implemented yet
	RunE: func(cmd *cobra.Command, args []string) error {
		// Previously a no-op that exited 0, pretending to succeed.
		return errors.New("`codefly list workspaces` is not implemented yet")
	},
}
