package provider

import (
	"context"

	"github.com/spf13/cobra"
)

var destroyCmd = &cobra.Command{
	Use:          "destroy BINDING",
	Short:        "Delete the owned or adopted remote resources of a binding",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	Long: `Delete a binding's remote resources.

Only resources the binding owns or has explicitly adopted are deleted, and only
when the binding declares deletion-policy: delete-owned. The default retain
policy never deletes remote resources.`,
	RunE: run(func(ctx context.Context, args []string) error {
		return gatedCommand(ctx, "destroy", envFlag, args[0])
	}),
}

func init() {
	destroyCmd.Flags().StringVar(&envFlag, "env", "local", "Environment the binding belongs to")
	destroyCmd.Flags().BoolVar(&jsonFlag, "json", false, "Print a machine-readable report to stdout")
}
