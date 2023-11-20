package cmd

import (
	"github.com/codefly-dev/cli/cmd/delete"
	"github.com/spf13/cobra"
)

// DeleteCmd represents the delete command
var DeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete",
}

func init() {
	DeleteCmd.AddCommand(delete.ProjectCmd)
}
