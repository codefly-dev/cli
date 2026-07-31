package provider

import (
	"context"

	"github.com/spf13/cobra"
)

var disconnectCmd = &cobra.Command{
	Use:          "disconnect BINDING",
	Short:        "Stop managing a binding without deleting its remote resources",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE: run(func(ctx context.Context, args []string) error {
		return gatedCommand(ctx, "disconnect", envFlag, args[0])
	}),
}

func init() {
	disconnectCmd.Flags().StringVar(&envFlag, "env", "local", "Environment the binding belongs to")
	disconnectCmd.Flags().BoolVar(&jsonFlag, "json", false, "Print a machine-readable report to stdout")
}
