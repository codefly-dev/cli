package install

import (
	"errors"

	"github.com/spf13/cobra"
)

// LibraryCmd represents the run command
var LibraryCmd = &cobra.Command{
	Use:    "library",
	Short:  "Install a library",
	Hidden: true, // not implemented yet
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("`codefly install library` is not implemented yet")
	},
}

var (
	destination string
	override    bool
)

func init() {
	LibraryCmd.PersistentFlags().StringVar(&destination, "destination", "", "Address of the library")
	LibraryCmd.PersistentFlags().BoolVar(&override, "override", false, "Replace destination directory")
}
