package provider

import (
	"context"

	"github.com/spf13/cobra"
)

var setupDryRun bool

var setupCmd = &cobra.Command{
	Use:          "setup BINDING",
	Short:        "Run the full setup lifecycle for a binding",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE: run(func(ctx context.Context, args []string) error {
		return gatedCommand(ctx, "setup", envFlag, args[0])
	}),
}

func init() {
	setupCmd.Flags().StringVar(&envFlag, "env", "local", "Environment the binding belongs to")
	setupCmd.Flags().BoolVar(&jsonFlag, "json", false, "Print a machine-readable report to stdout")
	setupCmd.Flags().BoolVar(&setupDryRun, "dry-run", false, "Validate and plan without applying any effect")
}
