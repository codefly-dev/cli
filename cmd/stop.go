package cmd

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/spf13/cobra"
)

func stopClearOptions() clearOptions {
	return clearOptions{verb: "stop", keepContainers: true, scope: reapScope(stopAllWorkspaces)}
}

// StopCmd stops a running codefly stack — the command muscle-memory reaches for.
// Its ABSENCE was a real footgun: `codefly stop` errored "unknown command" and
// silently did nothing, so service binaries kept running and held their ports,
// and the next run hit "address already in use" / cargo-lock deadlocks.
//
// `stop` kills codefly processes and reaps orphaned process groups (INCLUDING the
// service binaries under <module>/.cache/native that survive a SIGKILLed parent),
// but KEEPS stateful docker containers (postgres/redis/...) so the next run reuses
// them instead of paying a slow cold restart. For a full reset that also removes
// containers, use `codefly clear`.
//
// It is scoped to the CURRENT WORKSPACE. It was not, and that was its own
// footgun in the other direction: on a machine running one workspace per
// checkout — the normal shape for anyone working on more than one branch, and for
// any fleet of agents sharing a box — a `stop` in one workspace killed every
// other workspace's running services, including process groups it classified as
// "orphaned" and long-lived dev servers it had never launched. Nothing in the
// output said another workspace had been touched. `--all` is now how someone asks
// for the machine-wide sweep, and a scoped stop reports what it left alone.
var StopCmd = &cobra.Command{
	Use:     "stop [name-filter...]",
	Short:   "Stop this workspace's Codefly processes, preserving stateful containers for reuse",
	Aliases: []string{"down", "kill"},
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		// Same machinery as `clear`, but keep containers (reuse stateful infra).
		return clearCommand(ctx, args, stopClearOptions())
	},
}

func init() {
	StopCmd.Flags().BoolVar(&stopAllWorkspaces, "all", false, "Stop every workspace's processes on this machine, not just the current one")
}
