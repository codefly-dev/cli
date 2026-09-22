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

	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/cli/pkg/toolbox"
	"github.com/codefly-dev/core/wool"
)

// reapStaleProcessGroups self-heals process groups leaked by dead owners. It is
// a package variable so tests can drive its failure path; production binds the
// real reaper. Reaping is best-effort (see NewWorkspaceHost), never a
// construction precondition.
var reapStaleProcessGroups = processgroup.ReapStaleProcessGroups

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
	if err := reapStaleProcessGroups(reapContext(cfg.LogWriter)); err != nil {
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

// reapNarration sends what the reaper logs to the host's log sink, which is
// where the rest of this host's output already goes. It writes the line as
// logged rather than reusing logHostWarning: most of what the reaper emits is
// INFO, and the warning helper would label it as a host warning it is not.
type reapNarration struct {
	writer io.Writer
}

func (n reapNarration) Process(msg *wool.Log) {
	if msg.Level < wool.GlobalLogLevel() {
		return
	}
	writer := n.writer
	if writer == nil {
		writer = os.Stderr
	}
	fmt.Fprintln(writer, msg.String())
}

// reapContext carries that sink into the reaper, which otherwise runs on a
// bare context: with no provider on it, wool resolves to its process-global
// fallback — a console printing to stdout. mcp.ProtectStdout redirects that
// fallback, but only for `codefly mcp serve`; every other embedder of a
// WorkspaceHost still gets these lines on stdout, and one serving a protocol
// there has no way to reach them. Binding them to the host's own sink fixes it
// for all of them.
func reapContext(w io.Writer) context.Context {
	ctx := context.Background()
	provider := wool.New(ctx, &wool.Resource{Kind: "engine", Unique: "workspace-host"})
	provider.WithLogger(reapNarration{writer: w})
	return provider.Inject(ctx)
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

// SourceAt returns source behavior rooted at one exact directory inside this
// host. Parser-derived behavior is scoped to the code-unit root it is asked
// about, so it cannot borrow the host-wide root: an index or an import
// inventory taken from a parent directory would answer about a different tree.
//
// The caller owns the returned Source and must Close it. Roots arrive from
// request payloads, so a host-held cache keyed by them would grow for the life
// of a long-running gateway; construction only resolves the root, while every
// operation served here walks the tree anyway.
func (h *WorkspaceHost) SourceAt(root string) (*Source, error) {
	if h == nil {
		return nil, fmt.Errorf("workspace host is closed")
	}
	target, err := normalizeTarget(h.root, ServiceTarget{Root: root})
	if err != nil {
		return nil, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return nil, fmt.Errorf("workspace host is closed")
	}
	return newSource(target.Root), nil
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
