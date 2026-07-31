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
	RunE: run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		return gatedCommand(ctx, "disconnect", envOf(cmd), args[0], jsonOf(cmd))
	}),
}

func init() {
	addEnvJSONFlags(disconnectCmd, "Environment the binding belongs to")
}
