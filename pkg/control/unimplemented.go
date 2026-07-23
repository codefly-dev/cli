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

func (p *planeImpl) Addresses(ctx context.Context, service string) ([]Endpoint, error) {
	return nil, errNotImplemented
}

func (p *planeImpl) Logs(ctx context.Context, opts LogOptions, emit func(LogLine) error) error {
	return errNotImplemented
}

// --- Lifecycle (remaining) ---
// Build, Test, Run, Stop are implemented in lifecycle.go.

func (p *planeImpl) Lint(ctx context.Context, req CheckRequest) (CheckResult, error) {
	return CheckResult{}, errNotImplemented
}

func (p *planeImpl) Compile(ctx context.Context, req CheckRequest) (CheckResult, error) {
	return CheckResult{}, errNotImplemented
}

func (p *planeImpl) RunChecks(ctx context.Context, req CheckRequest) (CheckResult, error) {
	return CheckResult{}, errNotImplemented
}

// Deploy is implemented in deploy.go (gated by MutationAuthority).

// --- SourceEditor (remaining) ---
// File CRUD, ListFiles, Search, ApplyEdit, BatchApplyEdits are in source.go.

// Fix is language-aware repair — plugin behavior (Code plugin over gRPC), lifted
// with the Code-plugin group, not with the direct-filesystem source ops.
func (p *planeImpl) Fix(ctx context.Context, req FixRequest) (FixResult, error) {
	return FixResult{}, errNotImplemented
}

// --- VCS is implemented in vcs.go ---

// --- DependencyManager ---

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

// --- TerminalController ---

func (p *planeImpl) OpenTerminal(ctx context.Context, req OpenTerminalRequest) (TerminalID, error) {
	return "", errNotImplemented
}

func (p *planeImpl) AttachTerminal(ctx context.Context, id TerminalID, onOutput func([]byte) error) (TerminalInput, error) {
	return nil, errNotImplemented
}

func (p *planeImpl) ResizeTerminal(ctx context.Context, id TerminalID, cols, rows int) error {
	return errNotImplemented
}

func (p *planeImpl) CloseTerminal(ctx context.Context, id TerminalID) error {
	return errNotImplemented
}

func (p *planeImpl) ListTerminals(ctx context.Context) ([]TerminalID, error) {
	return nil, errNotImplemented
}

// --- MutationAuthority is implemented in mutation.go ---
