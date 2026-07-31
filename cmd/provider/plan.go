package provider

import (
	"context"

	"github.com/spf13/cobra"
)

var planCmd = &cobra.Command{
	Use:          "plan BINDING",
	Short:        "Calculate a deterministic plan for a binding",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE: run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		validateOnly, _ := cmd.Flags().GetBool("validate-only")
		refreshOnly, _ := cmd.Flags().GetBool("refresh-only")
		if validateOnly && refreshOnly {
			return errMutuallyExclusive("--validate-only", "--refresh-only")
		}
		return gatedCommand(ctx, "plan", envOf(cmd), args[0], jsonOf(cmd))
	}),
}

func init() {
	addEnvJSONFlags(planCmd, "Environment the binding belongs to")
	planCmd.Flags().Bool("validate-only", false, "Validate desired input without observing remote state")
	planCmd.Flags().Bool("refresh-only", false, "Refresh observation without calculating a plan")
	planCmd.Flags().String("out", "", "Write the calculated plan to this path")
}
