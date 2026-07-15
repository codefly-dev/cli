package cmd

import (
	"github.com/codefly-dev/cli/cmd/expose"
	"github.com/spf13/cobra"
)

// ExposeCmd represents the expose command
var ExposeCmd = &cobra.Command{
	Use:   "expose",
	Short: "Expose a service",
}

func init() {
	ExposeCmd.AddCommand(expose.ServiceCmd)
}
