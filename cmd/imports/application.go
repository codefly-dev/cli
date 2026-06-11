package imports

import (
	"errors"

	"github.com/spf13/cobra"
)

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:    "module",
	Short:  "Import a module",
	Hidden: true, // not implemented yet
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("`codefly import module` is not implemented yet")
	},
}

var path string

func init() {
	ModuleCmd.Flags().StringVarP(&path, "path", "p", "", "Path to the modules to import")
}
