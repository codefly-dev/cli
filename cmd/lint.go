package cmd

import (
	"github.com/codefly-dev/cli/cmd/validation"
	"github.com/spf13/cobra"
)

// LintCmd exposes plugin-owned lint outside CI.
var LintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Run plugin-owned lint checks for a service",
}

func init() {
	LintCmd.AddCommand(validation.LintServiceCmd)
}
