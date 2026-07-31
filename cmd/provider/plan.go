package provider

import (
	"context"

	"github.com/spf13/cobra"
)

var (
	planValidateOnly bool
	planRefreshOnly  bool
	planOut          string
)

var planCmd = &cobra.Command{
	Use:          "plan BINDING",
	Short:        "Calculate a deterministic plan for a binding",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE: run(func(ctx context.Context, args []string) error {
		if planValidateOnly && planRefreshOnly {
			return errMutuallyExclusive("--validate-only", "--refresh-only")
		}
		return gatedCommand(ctx, "plan", envFlag, args[0])
	}),
}

func init() {
	planCmd.Flags().StringVar(&envFlag, "env", "local", "Environment the binding belongs to")
	planCmd.Flags().BoolVar(&jsonFlag, "json", false, "Print a machine-readable report to stdout")
	planCmd.Flags().BoolVar(&planValidateOnly, "validate-only", false, "Validate desired input without observing remote state")
	planCmd.Flags().BoolVar(&planRefreshOnly, "refresh-only", false, "Refresh observation without calculating a plan")
	planCmd.Flags().StringVar(&planOut, "out", "", "Write the calculated plan to this path")
}
