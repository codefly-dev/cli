package list

import (
	"errors"

	"github.com/spf13/cobra"
)

// LibraryCmd represents the run command
var LibraryCmd = &cobra.Command{
	Use:    "libraries",
	Short:  "List all libraries for the workspace",
	Hidden: true, // not implemented yet
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("`codefly list libraries` is not implemented yet")
	},
}
