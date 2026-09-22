package cmd

import (
	"github.com/codefly-dev/cli/cmd/override"
	"github.com/spf13/cobra"
)

// OverrideCmd represents the override command
var OverrideCmd = &cobra.Command{
	Use:   "override",
	Short: "Point part of a composed module somewhere else on this machine",
}

func init() {
	OverrideCmd.AddCommand(override.ServiceCmd)
}
