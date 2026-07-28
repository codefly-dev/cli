package cmd

import (
	"github.com/codefly-dev/cli/cmd/self"
	"github.com/spf13/cobra"
)

// SelfCmd groups commands that operate on the codefly CLI itself.
var SelfCmd = &cobra.Command{
	Use:   "self",
	Short: "Maintain a local Codefly CLI checkout and installation",
}

func init() {
	SelfCmd.AddCommand(self.BuildCmd)
	SelfCmd.AddCommand(self.PullCmd)
	SelfCmd.AddCommand(self.CheckUpdateCmd)
	SelfCmd.AddCommand(self.UpdateCmd)
}
