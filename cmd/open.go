package cmd

import (
	"github.com/codefly-dev/cli/cmd/open"
	"github.com/spf13/cobra"
)

// OpenCmd represents the build command
var OpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open your project and application automatically",
}

func init() {
	OpenCmd.AddCommand(open.ProjectCmd)
	OpenCmd.AddCommand(open.ApplicationCmd)
	OpenCmd.AddCommand(open.ServiceCmd)
}
