package cmd

import (
	"github.com/codefly-dev/cli/cmd/self"
	"github.com/spf13/cobra"
)

// SelfCmd groups commands that operate on the codefly CLI itself.
var SelfCmd = &cobra.Command{
	Use:   "self",
	Short: "Manage the codefly CLI itself",
}

func init() {
	SelfCmd.AddCommand(self.BuildCmd)
	SelfCmd.AddCommand(self.PullCmd)
}
