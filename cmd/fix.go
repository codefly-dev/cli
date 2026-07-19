package cmd

import (
	"github.com/codefly-dev/cli/cmd/sourcefix"
	"github.com/spf13/cobra"
)

var FixCmd = &cobra.Command{
	Use:   "fix",
	Short: "Safely repair source through language plugins",
}

func init() {
	FixCmd.AddCommand(sourcefix.ServiceCmd)
	FixCmd.AddCommand(sourcefix.SourceCmd)
}
