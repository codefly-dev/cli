package cmd

import (
	"github.com/codefly-dev/cli/cmd/sourcefix"
	"github.com/spf13/cobra"
)

var FixCmd = &cobra.Command{
	Use:   "fix",
	Short: "Apply safe repairs to service source code and contracts",
}

func init() {
	FixCmd.AddCommand(sourcefix.ServiceCmd)
	FixCmd.AddCommand(sourcefix.SourceCmd)
	FixCmd.AddCommand(sourcefix.ProtoNamesCmd)
}
