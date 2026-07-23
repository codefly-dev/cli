package cmd

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/spf13/cobra"
)

func stopClearOptions() clearOptions {
	return clearOptions{verb: "stop", keepContainers: true}
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
var StopCmd = &cobra.Command{
	Use:     "stop [name-filter...]",
	Short:   "Stop Codefly processes while preserving stateful containers for reuse",
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
