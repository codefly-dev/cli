package provider

import (
	"context"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
	"github.com/spf13/cobra"
)

const applyCommandName = "apply"

var applyPlan string

var applyCmd = &cobra.Command{
	Use:          applyCommandName,
	Short:        "Execute a previously calculated plan",
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	RunE: run(func(_ context.Context, _ []string) error {
		if applyPlan == "" {
			return errRequiredFlag("--plan")
		}
		// A plan is produced by the coordinator, which is not wired in this
		// build, so no plan file can exist to apply. Fail closed.
		result := hostprovider.NewResult(applyCommandName, "")
		result.Fail(hostprovider.CodeCoordinatorUnavailable, "applying a plan requires the host coordinator, not available in this build")
		return result.Emit(jsonFlag)
	}),
}

func init() {
	applyCmd.Flags().StringVar(&applyPlan, "plan", "", "Path to a plan produced by `codefly provider plan --out`")
	applyCmd.Flags().BoolVar(&jsonFlag, "json", false, "Print a machine-readable report to stdout")
}
