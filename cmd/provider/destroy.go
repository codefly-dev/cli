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
	RunE: run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		return gatedCommand(ctx, "destroy", envOf(cmd), args[0], jsonOf(cmd))
	}),
}

func init() {
	addEnvJSONFlags(destroyCmd, "Environment the binding belongs to")
}
