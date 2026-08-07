// Package engine owns the transport-independent Codefly runtime behavior.
//
// A WorkspaceHost is explicitly rooted and owns every process it starts. Thin
// adapters (Cobra, Gateway gRPC, MCP, and in-process callers) bind requests to a
// Service and delegate here instead of reimplementing agent lifecycle.
package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/codefly-dev/cli/pkg/toolbox"
	runnersbase "github.com/codefly-dev/core/runners/base"
)

// reapStaleProcessGroups self-heals process groups leaked by dead owners. It is
// a package variable so tests can drive its failure path; production binds the
// real reaper. Reaping is best-effort (see NewWorkspaceHost), never a
// construction precondition.
var reapStaleProcessGroups = runnersbase.ReapStaleProcessGroups

// Config configures a WorkspaceHost.
type Config struct {
	// Root is the explicit filesystem boundary for this host. It is never
	// inferred from the process working directory.
	Root string
	// LogWriter receives prefixed agent stderr. Nil discards live agent logs;
	// startup failures still retain the agent manager's bounded stderr tail.
	LogWriter io.Writer
}

// WorkspaceHost owns service-agent processes rooted in one workspace or source
// tree. It is safe for concurrent use.
type WorkspaceHost struct {
	root       string
	supervisor *AgentSupervisor
	source     *Source
	flows      *FlowManager
	tools      *toolbox.Registry

	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
}

// NewWorkspaceHost creates an explicitly rooted host.
func NewWorkspaceHost(cfg Config) (*WorkspaceHost, error) {
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		return nil, fmt.Errorf("workspace host root is required")
	}
	// RECOVERY BOUNDARY: WorkspaceHost is the shared process-owning entry point
	// for the CLI, MCP, Gateway gRPC, and Mind's in-process Gateway. Reap PGIDs
	// left by dead owners here so every front door self-heals after a crash; the
	// reaper preserves groups whose owning process is still alive.
	//
	// Reaping is best-effort: a failure (signal permission, an unusual runs
	// directory, a race with a concurrent host) must not stop every front door
	// from starting. Surface it and continue rather than failing construction.
	if err := reapStaleProcessGroups(context.Background()); err != nil {
		logHostWarning(cfg.LogWriter, fmt.Sprintf("could not reap stale workspace processes: %v", err))
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace host root: %w", err)
	}
	absolute = filepath.Clean(absolute)
	absolute, info, err := canonicalPath(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace host root: %w", err)
	}
	if info != nil && !info.IsDir() {
		return nil, fmt.Errorf("workspace host root is not a directory: %s", absolute)
	}
	supervisor := NewAgentSupervisor(AgentSupervisorConfig{
		Root:      absolute,
		LogWriter: cfg.LogWriter,
	})
	return &WorkspaceHost{
		root:       absolute,
		supervisor: supervisor,
		source:     newSource(absolute),
		flows:      NewFlowManager(),
		tools:      toolbox.NewRegistry(),
	}, nil
}

// logHostWarning surfaces a non-fatal host warning to the configured log sink,
// falling back to stderr so a best-effort failure is never silently swallowed.
func logHostWarning(w io.Writer, message string) {
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "codefly workspace host: %s\n", message)
}

// Root returns the immutable absolute root owned by this host.
func (h *WorkspaceHost) Root() string {
	if h == nil {
		return ""
	}
	return h.root
}

// Service binds leaf behavior to one service target. Binding does not start an
// agent; startup remains lazy until an operation needs one.
func (h *WorkspaceHost) Service(target ServiceTarget) (*Service, error) {
	if h == nil {
		return nil, fmt.Errorf("workspace host is closed")
	}
	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return nil, fmt.Errorf("workspace host is closed")
	}
	supervisor := h.supervisor
	h.mu.RUnlock()
	normalized, err := normalizeTarget(h.root, target)
	if err != nil {
		return nil, err
	}
	return &Service{target: normalized, supervisor: supervisor}, nil
}

// Source returns the host's language-neutral code behavior. It is useful for
// source-only repositories that do not yet have Codefly service metadata.
func (h *WorkspaceHost) Source() *Source {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return nil
	}
	return h.source
}

// Flows returns the registry of orchestration flows owned by this host.
func (h *WorkspaceHost) Flows() *FlowManager {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return nil
	}
	return h.flows
}

// Toolbox returns the transport-neutral tool registry owned by this host.
func (h *WorkspaceHost) Toolbox() *toolbox.Registry {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return nil
	}
	return h.tools
}

// Close stops every flow and agent process owned by the host. It is idempotent.
func (h *WorkspaceHost) Close() error {
	if h == nil {
		return nil
	}
	var closeErr error
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		flows, tools := h.flows, h.tools
		supervisor, source := h.supervisor, h.source
		h.mu.Unlock()

		if flows != nil {
			closeErr = flows.Close()
		}
		if tools != nil {
			tools.Close()
		}
		if supervisor != nil {
			supervisor.Close()
		}
		if source != nil {
			_ = source.Close()
		}
	})
	return closeErr
}
