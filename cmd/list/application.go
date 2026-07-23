package list

import (
	"errors"

	"github.com/spf13/cobra"
)

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:    "modules",
	Short:  "List modules declared by the current workspace",
	Hidden: true, // not implemented yet
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("`codefly list modules` is not implemented yet")
	},
}
