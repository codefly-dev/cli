package provider

import (
	"context"

	"github.com/spf13/cobra"
)

var setupCmd = &cobra.Command{
	Use:          "setup BINDING",
	Short:        "Run the full setup lifecycle for a binding",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE: run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		return gatedCommand(ctx, "setup", envOf(cmd), args[0], jsonOf(cmd))
	}),
}

func init() {
	addEnvJSONFlags(setupCmd, "Environment the binding belongs to")
	setupCmd.Flags().Bool("dry-run", false, "Validate and plan without applying any effect")
}
