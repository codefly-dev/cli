package cmd

import (
	"github.com/codefly-dev/cli/cmd/context"
	"github.com/spf13/cobra"
)

// ContextCmd represents the Context command
var ContextCmd = &cobra.Command{
	Use:   "context",
	Short: "Codefly Context",
}

func init() {
	ContextCmd.AddCommand(context.AllCmd)
	ContextCmd.AddCommand(context.CurrentCmd)
	ContextCmd.AddCommand(context.SwitchCmd)
}
