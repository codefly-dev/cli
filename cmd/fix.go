package cmd

import (
	"github.com/codefly-dev/cli/cmd/sourcefix"
	"github.com/spf13/cobra"
)

var FixCmd = &cobra.Command{
	Use:   "fix",
	Short: "Apply safe, plugin-owned repairs to service source code",
}

func init() {
	FixCmd.AddCommand(sourcefix.ServiceCmd)
	FixCmd.AddCommand(sourcefix.SourceCmd)
}
