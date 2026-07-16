package cmd

import (
	"github.com/codefly-dev/cli/cmd/ci"
	"github.com/spf13/cobra"
)

// CiCmd represents the add command
var CiCmd = &cobra.Command{
	Use:   "ci",
	Short: "ci",
}

func init() {
	CiCmd.AddCommand(ci.PlanCmd)
	CiCmd.AddCommand(ci.RunCmd)
	CiCmd.AddCommand(ci.LintCmd)
	CiCmd.AddCommand(ci.CompileCmd)
	CiCmd.AddCommand(ci.TestCmd)
	CiCmd.AddCommand(ci.BuildCmd)
	CiCmd.AddCommand(ci.DeployCmd)
	CiCmd.AddCommand(ci.PushCmd)
}
