package control

import (
	"context"
	"errors"
)

// errNotImplemented marks a Plane capability that has been declared on the
// interface but not yet lifted from a legacy surface (pkg/web, pkg/mcp,
// pkg/gateway) into this single implementation. Each Phase-1 lift replaces one
// of these with a real method in a dedicated file (impl.go and siblings).
var errNotImplemented = errors.New("control: capability not yet lifted into the control plane")

// The methods below keep planeImpl satisfying the full Plane interface while the
// groups are lifted incrementally. They are grouped by capability to match the
// interfaces in plane.go; delete each as its real implementation lands.

// --- Introspector (remaining) ---

// Addresses, Lint, Compile, RunChecks are implemented in checks.go.

// Logs remains stubbed intentionally. Logs are produced by a PROCESS-GLOBAL
// agents processor feeding an in-memory channel (see pkg/web's server). Owning
// that processor requires the plane to be a singleton for the process, which
// conflicts with New() being a lightweight per-caller constructor. Wiring it is
// a decision for the server-adapter phase (one long-lived plane owns the log
// channel); until then this reports not-implemented rather than silently
// returning no logs.
func (p *planeImpl) Logs(ctx context.Context, opts LogOptions, emit func(LogLine) error) error {
	return errNotImplemented
}

// --- Lifecycle (remaining) ---
// Build, Test, Run, Stop are implemented in lifecycle.go.
// Deploy is implemented in deploy.go (gated by MutationAuthority).

// --- SourceEditor (remaining) ---
// File CRUD, ListFiles, Search, ApplyEdit, BatchApplyEdits are in source.go.
// Fix (language-aware plugin repair) is in plugin.go.

// --- VCS is implemented in vcs.go ---

// --- DependencyManager is implemented in plugin.go ---

// --- CommandRunner is implemented in commands.go ---

// --- TerminalController is implemented in terminal.go ---

// --- MutationAuthority is implemented in mutation.go ---
