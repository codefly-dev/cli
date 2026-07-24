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

// Fix is language-aware plugin repair via the Code plugin's Execute RPC. Doing
// it correctly requires spinning up the plugin through the runtime Load/Init
// lifecycle so the plugin resolves the SERVICE source tree (SourceLocation) —
// merely dialing a Code client with WithWorkDir(workspace) makes it operate on
// the wrong directory. That lifecycle is not lifted yet, so this stays stubbed.
func (p *planeImpl) Fix(ctx context.Context, req FixRequest) (FixResult, error) {
	return FixResult{}, errNotImplemented
}

// --- VCS is implemented in vcs.go ---

// --- DependencyManager ---
// Same as Fix: these run through the Code plugin and need the runtime Load/Init
// lifecycle to resolve the service source dir before add/remove/list operate on
// the right go.mod. Not lifted yet.

func (p *planeImpl) ListDependencies(ctx context.Context, service string) ([]Dependency, error) {
	return nil, errNotImplemented
}

func (p *planeImpl) AddDependency(ctx context.Context, service string, dep Dependency) error {
	return errNotImplemented
}

func (p *planeImpl) RemoveDependency(ctx context.Context, service string, dep Dependency) error {
	return errNotImplemented
}

// --- CommandRunner is implemented in commands.go ---

// --- TerminalController is implemented in terminal.go ---

// --- MutationAuthority is implemented in mutation.go ---
