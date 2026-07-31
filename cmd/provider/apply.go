package provider

import (
	"context"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
	"github.com/spf13/cobra"
)

const applyCommandName = "apply"

var applyCmd = &cobra.Command{
	Use:          applyCommandName,
	Short:        "Execute a previously calculated plan",
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	RunE: run(func(_ context.Context, cmd *cobra.Command, _ []string) error {
		plan, _ := cmd.Flags().GetString("plan")
		if plan == "" {
			return errRequiredFlag("--plan")
		}
		// A plan is produced by the coordinator, which is not wired in this
		// build, so no plan file can exist to apply. Fail closed.
		result := hostprovider.NewResult(applyCommandName, "")
		result.Fail(hostprovider.CodeCoordinatorUnavailable, "applying a plan requires the host coordinator, not available in this build")
		return result.Emit(jsonOf(cmd))
	}),
}

func init() {
	applyCmd.Flags().String("plan", "", "Path to a plan produced by `codefly provider plan --out`")
	addJSONFlag(applyCmd)
}
