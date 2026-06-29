package cmd

import (
	"github.com/spf13/cobra"
)

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
	Short:   "Stop codefly processes + reap orphaned groups (keeps stateful containers; use `clear` for a full reset)",
	Aliases: []string{"down", "kill"},
	Run: func(cmd *cobra.Command, args []string) {
		// Same machinery as `clear`, but keep containers (reuse stateful infra).
		clearVerb = "stop"
		clearKeepContainers = true
		clearCommand(args)
	},
}
