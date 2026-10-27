// Package runnables owns the projection of a resources.Runnable that the CLI's
// listing and inspection surfaces emit.
//
// There is one projection, not one per surface. `codefly list runnables
// --json` and the MCP `list_runnables` tool answer the same question for a
// human and for an agent, and `codefly show runnable --json` answers a
// superset of it; hand-maintaining that shape three times means the three
// answers drift, which is exactly what they had already done.
package runnables

import "github.com/codefly-dev/core/resources"

// Execution is a runnable's declared execution bounds. MaxInputBytes and
// MaxOutputBytes are the EFFECTIVE bounds — core substitutes a default for an
// undeclared payload limit, and reporting the blank instead of the bound that
// will apply tells a caller nothing about what its payload must fit in.
type Execution struct {
	Facilities     []string `json:"facilities"`
	Timeout        string   `json:"timeout"`
	Cancellation   string   `json:"cancellation"`
	Recovery       string   `json:"recovery"`
	Concurrency    uint32   `json:"concurrency,omitempty"`
	MaxInputBytes  uint64   `json:"max_input_bytes"`
	MaxOutputBytes uint64   `json:"max_output_bytes"`
}

// Identity is the immutable module/name@version release identity plus the
// facts that identify what builds and speaks to it.
type Identity struct {
	Module      string    `json:"module"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	Agent       string    `json:"agent"`
	Protocol    string    `json:"protocol"`
	Execution   Execution `json:"execution"`
	// Source is the method a derived operation was projected from, as
	// "<service>/<endpoint>/<Method>". It is empty for an authored runnable,
	// which is the distinction a reader needs: a derived row is regenerated
	// from a contract, not edited.
	Source string `json:"source,omitempty"`
	// Tool is the exposure the owner declared, nil when the operation is no
	// tool. Absent means not exposed: that is the owner's fail-closed default
	// and this listing repeats it rather than inventing one.
	Tool *Tool `json:"tool,omitempty"`
	// Grant is how one environment answers this operation, set only when a
	// listing was asked about an environment.
	Grant *Grant `json:"grant,omitempty"`
}

// NewIdentity projects a loaded runnable. The runnable must have come from
// core's loader, which validates that Agent, Contract and Execution are all
// present before it returns one, so this dereferences them directly rather
// than reporting a half-empty row for a declaration that cannot exist.
func NewIdentity(runnable *resources.Runnable) Identity {
	return Identity{
		Module:      runnable.Module(),
		Name:        runnable.Name,
		Version:     runnable.Version,
		Description: runnable.Description,
		Agent:       runnable.Agent.Identifier(),
		Protocol:    runnable.Contract.Protocol,
		Execution:   NewExecution(runnable.Execution),
	}
}

// NewExecution projects the declared execution bounds.
func NewExecution(execution *resources.RunnableExecution) Execution {
	facilities := make([]string, 0, len(execution.Facilities))
	for _, facility := range execution.Facilities {
		facilities = append(facilities, string(facility))
	}
	return Execution{
		Facilities:     facilities,
		Timeout:        execution.Timeout,
		Cancellation:   string(execution.Cancellation),
		Recovery:       string(execution.Recovery),
		Concurrency:    execution.Concurrency,
		MaxInputBytes:  execution.MaxInputBytes(),
		MaxOutputBytes: execution.MaxOutputBytes(),
	}
}

// Tool is how one operation is offered to a model, as the owner declared it
// (codefly.runnable.v0.ToolExposure). It is what a composition picks from: a
// name, a bounded description and an explicit effect, because an operation
// with no exposure is not a tool and an unstated effect is not read-only.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Effect is EFFECT_READ_ONLY or EFFECT_MUTATION, spelled as the enum is.
	Effect string `json:"effect"`
}

// Grant is what one environment has answered about one derived operation: the
// configuration key the answer is written under, whether a binding is prepared
// for it, and — when it is not — which of the owner's scope slots are still
// open. A module version that adds an operation adds an unanswered row, which
// is how "this version offers a tool nobody granted" becomes visible without
// comparing two releases by hand.
type Grant struct {
	// Key is the runnable-bindings key, which is also the
	// runnable-scope-selections key an answer is written under.
	Key string `json:"key"`
	// State is `selected`, `declined`, or `needs-scopes`.
	State string `json:"state"`
	// Slots names the owner's required scope slots, when the operation
	// declares any: the questions an answer must resolve.
	Slots []string `json:"slots,omitempty"`
}

// Grant states, which are the whole vocabulary a listing reports.
const (
	// GrantSelected is an operation this environment prepares a binding for:
	// either it needs no slot answered, or its answer resolves them.
	GrantSelected = "selected"
	// GrantDeclined is an operation answered with an empty list: installed
	// for no resource, granted no authority, offered as no tool.
	GrantDeclined = "declined"
	// GrantNeedsScopes is an operation whose owner asks for scope slots this
	// environment has not answered. Generation refuses it by name rather than
	// preparing a binding with the question still open, so an unanswered slot
	// is work to do, not a quiet omission.
	GrantNeedsScopes = "needs-scopes"
)
