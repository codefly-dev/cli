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
