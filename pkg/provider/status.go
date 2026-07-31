// Package provider implements the Codefly host side of the external provider
// lifecycle: environment bindings, the `codefly provider` command contract,
// and the static (no-agent) validation and doctor checks.
//
// This package is the foundation slice of that lifecycle. It owns the binding
// model, its offline validation, and the stable exit-code/JSON contract shared
// by every provider command. The host coordinator, hardened provider launch,
// broker observation, durable state, projection writer, and signed receipts
// are separate, still-unbuilt layers; commands that require them fail closed
// through Gated rather than pretending to succeed.
package provider

// Status is the stable, machine-readable outcome of a provider command. Each
// value maps one-to-one to a process exit code so that scripts and CI can
// branch on the class of outcome without parsing prose.
type Status string

const (
	// StatusSuccess is success, a no-diff plan, or a completed apply. Exit 0.
	StatusSuccess Status = "success"
	// StatusInvalid is an invalid configuration, an incompatible provider, or
	// any otherwise unclassified failure. Exit 1.
	StatusInvalid Status = "invalid"
	// StatusDiff reports a valid, non-empty plan awaiting apply. Exit 2.
	StatusDiff Status = "diff"
	// StatusPolicyDenied reports that policy denied the operation. Exit 3.
	StatusPolicyDenied Status = "policy_denied"
	// StatusApprovalRequired reports that the operation needs approval it did
	// not have. Exit 4.
	StatusApprovalRequired Status = "approval_required"
	// StatusPartial reports that some effects landed and some did not. Exit 5.
	StatusPartial Status = "partial"
	// StatusUncertain reports a non-idempotent effect whose outcome is unknown
	// and must not be blindly retried. Exit 6.
	StatusUncertain Status = "uncertain"
	// StatusStale reports a plan, state, or endpoint that no longer matches the
	// world it was calculated against. Exit 7.
	StatusStale Status = "stale"
)

// ExitCode is the stable process status for an outcome. The mapping is a
// contract: it must never change value for an existing Status.
func (s Status) ExitCode() int {
	switch s {
	case StatusSuccess:
		return 0
	case StatusDiff:
		return 2
	case StatusPolicyDenied:
		return 3
	case StatusApprovalRequired:
		return 4
	case StatusPartial:
		return 5
	case StatusUncertain:
		return 6
	case StatusStale:
		return 7
	default:
		return 1
	}
}
