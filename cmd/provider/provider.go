// Package provider implements the `codefly provider` command group: the CLI
// surface of the external-provider lifecycle. The offline commands (list,
// binding validation, the static half of doctor) are fully implemented here.
// The commands that drive a live provider — setup, plan, apply, import,
// disconnect, destroy — validate their bindings offline and then fail closed
// through the coordinator-unavailable path, because the host coordinator,
// hardened launch, broker, durable state, projection, and receipts are separate
// layers that are not yet wired.
package provider

import (
	"github.com/spf13/cobra"
)

// CommandName is this command group's name, used in error prefixes and output.
const CommandName = "provider"

// Cmd is the `codefly provider` command group.
var Cmd = &cobra.Command{
	Use:   CommandName,
	Short: "Manage external provider bindings for an environment",
	Long: `Manage external provider bindings declared in provider-bindings.codefly.yaml.

A binding names an exact provider agent, a management mode (observe, managed, or
disabled), the public inputs and secret references it collects, and the output
configuration contract it projects into. Bindings are environment-scoped: select
one with --env.

Every command prints a deterministic report with --json and never emits a secret
value or a raw provider body. Exit codes are stable:

  0  success, no diff, or apply complete
  1  invalid configuration, incompatible provider, or unclassified failure
  2  a valid plan diff is present
  3  policy denied the operation
  4  the operation requires approval
  5  a partial outcome (some effects landed)
  6  an uncertain outcome that must not be blindly retried
  7  a stale plan, state, or endpoint`,
}

func init() {
	Cmd.AddCommand(listCmd, setupCmd, planCmd, applyCmd, doctorCmd, importCmd, disconnectCmd, destroyCmd)
}
