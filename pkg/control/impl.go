package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// planeImpl is the single implementation of Plane. Its root and runtime
// ownership are fixed at construction; operations never consult process CWD or
// a process-global flow registry after that point.
type planeImpl struct {
	root     string
	host     *engine.WorkspaceHost
	ownsHost bool
	initErr  error
	// gate enforces the mutation-authority policy for destructive/outward
	// actions (Deploy and, under prepared authority, edits). See mutation.go.
	gate *mutationGate
	// terminals holds this plane's live PTY sessions. See terminal.go.
	terminals *terminalManager
	closeOnce sync.Once
	closeErr  error
	// runMu guards lastRun, which records how the most recently active flow
	// ended after it is no longer in the flow registry (FlowManager forgets a
	// flow the instant it stops, so this is the only place FlowStatus can find
	// FlowFailed/FlowStopped once the flow is gone). See lifecycle.go's Run.
	runMu   sync.Mutex
	lastRun *runOutcome
	// runsMu guards runs: the flows Run started whose goroutines have not
	// returned yet, keyed by flow id. A flow id can hold more than one at a
	// time — the registry frees the id at Release, while the goroutine that
	// held it is still unwinding — so this maps to every run under that id
	// rather than only the newest. See activeRun and lifecycle.go's Run.
	runsMu sync.Mutex
	runs   map[string][]*activeRun
}

// runOutcome is how a flow, once no longer the registry's active flow, ended.
type runOutcome struct {
	flowID string
	state  FlowState
	err    error
}

// recordRunOutcome remembers how a flow ended after Run's background goroutine
// releases it from the registry, so a later FlowStatus call can still report
// FlowFailed/FlowStopped instead of falling back to FlowIdle.
func (p *planeImpl) recordRunOutcome(flowID string, state FlowState, err error) {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	p.lastRun = &runOutcome{flowID: flowID, state: state, err: err}
}

// clearRunOutcome discards any remembered outcome. Called when a new Run
// starts, so a stale failure from a previous, unrelated run never leaks into
// the new run's FlowStatus before it has reported anything itself.
func (p *planeImpl) clearRunOutcome() {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	p.lastRun = nil
}

func (p *planeImpl) runOutcome() *runOutcome {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	return p.lastRun
}

// activeRun is a flow Run started in the background: the cancel that releases
// its goroutine, and a channel closed once that goroutine has returned.
//
// That goroutine narrates as it unwinds — Playbook.Work logs on its way out —
// and it returns only when its context is done, which neither tearing the flow
// down nor closing the host does. Left untracked it outlives both Stop and
// Close and keeps writing to the caller's stdout, which for a plane serving
// JSON-RPC there is the protocol stream.
type activeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// runJoinBudget bounds how long joinRuns waits for cancelled goroutines to
// return. A run unwinds through orchestration's own teardown, which is budgeted
// per layer (15s to Stop, 30s to Shutdown) and so is bounded by that budget
// times the graph's depth — but only while it keeps making progress. Playbook's
// work loop observes cancellation between action groups, never during one, so a
// policy action that ignores its context would block the join indefinitely and
// turn a leaked goroutine into a hung stop_flow tool and a host that never
// closes. The budget clears a healthy teardown by a wide margin; exceeding it
// means something is genuinely wedged, which is reported rather than waited on.
const runJoinBudget = 90 * time.Second

// trackRun records a started run so Stop and Close can join its goroutine.
// Runs accumulate under their flow id rather than replacing each other: the
// registry releases the id as soon as the flow exits, so a fresh Run can claim
// it while the previous goroutine is still unwinding, and overwriting here
// would drop the only handle that could ever join that goroutine.
func (p *planeImpl) trackRun(flowID string, cancel context.CancelFunc) *activeRun {
	run := &activeRun{cancel: cancel, done: make(chan struct{})}
	p.runsMu.Lock()
	defer p.runsMu.Unlock()
	if p.runs == nil {
		p.runs = make(map[string][]*activeRun)
	}
	p.runs[flowID] = append(p.runs[flowID], run)
	return run
}

// finishRun marks a run's goroutine as returned, forgetting that run and
// leaving any sibling still running under the same flow id tracked.
func (p *planeImpl) finishRun(flowID string, run *activeRun) {
	p.runsMu.Lock()
	tracked := p.runs[flowID]
	kept := make([]*activeRun, 0, len(tracked))
	for _, candidate := range tracked {
		if candidate != run {
			kept = append(kept, candidate)
		}
	}
	if len(kept) == 0 {
		delete(p.runs, flowID)
	} else {
		p.runs[flowID] = kept
	}
	p.runsMu.Unlock()
	close(run.done)
}

// stopRun cancels every run under one flow id and waits for them. The wait is
// deliberately outside the lock: those goroutines take runMu to record their
// outcome, and holding a lock across the join invites a deadlock against them.
func (p *planeImpl) stopRun(flowID string) {
	p.runsMu.Lock()
	pending := append([]*activeRun(nil), p.runs[flowID]...)
	p.runsMu.Unlock()
	joinRuns(pending)
}

// stopAllRuns cancels every tracked run and waits for all of them.
func (p *planeImpl) stopAllRuns() {
	p.runsMu.Lock()
	pending := make([]*activeRun, 0, len(p.runs))
	for _, runs := range p.runs {
		pending = append(pending, runs...)
	}
	p.runsMu.Unlock()
	joinRuns(pending)
}

// joinRuns cancels every run before waiting on any of them. The two passes are
// the point: cancelling inside the waiting loop leaves run N+1 running until
// run N has finished unwinding, so closing a plane with several live flows
// costs the sum of their teardowns instead of the longest one.
//
// A run that outlives the budget is reported on stderr and abandoned. stderr,
// not stdout: the descriptor this is protecting may be carrying JSON-RPC, and
// a diagnostic about protocol corruption must not be the thing that causes it.
func joinRuns(pending []*activeRun) {
	for _, run := range pending {
		run.cancel()
	}
	budget := time.NewTimer(runJoinBudget)
	defer budget.Stop()
	for _, run := range pending {
		select {
		case <-run.done:
		case <-budget.C:
			wedged := 0
			for _, candidate := range pending {
				select {
				case <-candidate.done:
				default:
					wedged++
				}
			}
			fmt.Fprintf(os.Stderr,
				"codefly: %d run goroutine(s) did not return within %s and may still be writing output\n",
				wedged, runJoinBudget)
			return
		}
	}
}

// discardNarration drops every log handed to it.
type discardNarration struct{}

func (discardNarration) Process(*wool.Log) {}

// narrationContext returns ctx carrying a wool provider that discards output.
//
// The plane renders nothing itself — its one production caller serves JSON-RPC
// on stdout — but without a provider on the context wool.Get falls back to a
// Console that prints exactly there, so the narration the plane deliberately
// does not render reached the protocol stream anyway. Discarding matches the
// contract the plane already documents: Flow.WithOutputSink is the seam for an
// embedder that wants this output, and unset it is silently discarded.
func (p *planeImpl) narrationContext(ctx context.Context) context.Context {
	return wool.New(ctx, resources.CLI.AsResource()).WithLogger(discardNarration{}).Inject(ctx)
}

// New returns a control plane rooted at the current directory as observed once,
// at construction. Prefer NewAt or NewWithHost in long-lived adapters.
func New() Plane {
	root, err := os.Getwd()
	if err != nil {
		return &planeImpl{initErr: fmt.Errorf("resolve control-plane root: %w", err), gate: newMutationGate(), terminals: newTerminalManager()}
	}
	plane, err := NewAt(root)
	if err != nil {
		return &planeImpl{root: root, initErr: err, gate: newMutationGate(), terminals: newTerminalManager()}
	}
	return plane
}

// NewAt creates a control plane and its owning host at an explicit root.
func NewAt(root string) (Plane, error) {
	host, err := engine.NewWorkspaceHost(engine.Config{Root: root})
	if err != nil {
		return nil, err
	}
	return &planeImpl{
		root:      host.Root(),
		host:      host,
		ownsHost:  true,
		gate:      newMutationGate(),
		terminals: newTerminalManager(),
	}, nil
}

// Close releases every resource owned by this plane. NewWithHost callers keep
// ownership of the shared host and close it at their adapter boundary.
func (p *planeImpl) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		// Release the background runs first: their goroutines narrate as they
		// unwind, and a closed plane must not still be writing to the caller's
		// stdout. Cancelling before waiting keeps this from depending on the
		// caller having cancelled the context it passed to Run.
		p.stopAllRuns()
		if p.terminals != nil {
			p.terminals.close()
		}
		if p.ownsHost && p.host != nil {
			p.closeErr = p.host.Close()
		}
	})
	return p.closeErr
}

// NewWithHost binds a control plane to an existing runtime owner.
func NewWithHost(host *engine.WorkspaceHost) Plane {
	if host == nil {
		return &planeImpl{initErr: fmt.Errorf("workspace host is required"), gate: newMutationGate(), terminals: newTerminalManager()}
	}
	return &planeImpl{root: host.Root(), host: host, gate: newMutationGate(), terminals: newTerminalManager()}
}

func newPlaneRooted(root string) *planeImpl {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return &planeImpl{root: root, initErr: fmt.Errorf("resolve control-plane root: %w", err), gate: newMutationGate(), terminals: newTerminalManager()}
	}
	return &planeImpl{root: filepath.Clean(absolute), gate: newMutationGate(), terminals: newTerminalManager()}
}

// Compile-time proof that planeImpl satisfies the full control surface.
var _ Plane = (*planeImpl)(nil)

// workspace loads the workspace containing the plane's immutable root.
func (p *planeImpl) workspace(ctx context.Context) (*resources.Workspace, error) {
	if p.initErr != nil {
		return nil, p.initErr
	}
	root, err := workspaceRootFrom(p.root)
	if err != nil {
		return nil, err
	}
	ws, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	return ws, nil
}

func workspaceRootFrom(start string) (string, error) {
	if start == "" {
		return "", fmt.Errorf("control-plane root is required")
	}
	current := filepath.Clean(start)
	for {
		config := filepath.Join(current, resources.WorkspaceConfigurationName)
		if info, err := os.Stat(config); err == nil && !info.IsDir() {
			return current, nil
		} else if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect workspace configuration %s: %w", config, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no workspace found from %s", start)
		}
		current = parent
	}
}

// --- Introspector ---

// Inventory lists resource NAMES only. It deliberately does not use
// architecture.LoadWorkspace (which spins up every service's plugin runtime to
// collect endpoints) — listing names must stay cheap and side-effect free.
func (p *planeImpl) Inventory(ctx context.Context) (Inventory, error) {
	ws, err := p.workspace(ctx)
	if err != nil {
		return Inventory{}, err
	}
	inv := Inventory{Workspace: ws.Name, Description: ws.Description}
	modules, err := ws.LoadModules(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("load modules: %w", err)
	}
	agents := map[string]struct{}{}
	for _, mod := range modules {
		inv.Modules = append(inv.Modules, mod.Name)
		services, err := mod.LoadServices(ctx)
		if err != nil {
			return Inventory{}, fmt.Errorf("load services for module %s: %w", mod.Name, err)
		}
		for _, svc := range services {
			inv.Services = append(inv.Services, mod.Name+"/"+svc.Name)
			if svc.Agent != nil {
				agents[svc.Agent.Publisher+"/"+svc.Agent.Name] = struct{}{}
			}
		}
		jobs, err := mod.LoadJobs(ctx)
		if err != nil {
			return Inventory{}, fmt.Errorf("load jobs for module %s: %w", mod.Name, err)
		}
		for _, job := range jobs {
			inv.Jobs = append(inv.Jobs, mod.Name+"/"+job.Name)
		}
	}
	for agent := range agents {
		inv.Agents = append(inv.Agents, agent)
	}
	sort.Strings(inv.Agents)
	return inv, nil
}

// Modules lists the workspace's modules with descriptions.
func (p *planeImpl) Modules(ctx context.Context) ([]ModuleInfo, error) {
	ws, err := p.workspace(ctx)
	if err != nil {
		return nil, err
	}
	modules, err := ws.LoadModules(ctx)
	if err != nil {
		return nil, fmt.Errorf("load modules: %w", err)
	}
	infos := make([]ModuleInfo, 0, len(modules))
	for _, mod := range modules {
		infos = append(infos, ModuleInfo{Name: mod.Name, Description: mod.Description})
	}
	return infos, nil
}

// Services lists services with detail, optionally filtered to one module.
func (p *planeImpl) Services(ctx context.Context, module string) ([]ServiceDetail, error) {
	ws, err := p.workspace(ctx)
	if err != nil {
		return nil, err
	}
	modules, err := ws.LoadModules(ctx)
	if err != nil {
		return nil, fmt.Errorf("load modules: %w", err)
	}
	var details []ServiceDetail
	for _, mod := range modules {
		if module != "" && mod.Name != module {
			continue
		}
		services, err := mod.LoadServices(ctx)
		if err != nil {
			return nil, fmt.Errorf("load services for module %s: %w", mod.Name, err)
		}
		for _, svc := range services {
			detail := ServiceDetail{
				Module:      mod.Name,
				Name:        svc.Name,
				Description: svc.Description,
				Version:     svc.Version,
			}
			if svc.Agent != nil {
				detail.Agent = svc.Agent.Name
			}
			for _, ep := range svc.Endpoints {
				detail.Endpoints = append(detail.Endpoints, DeclaredEndpoint{
					Name:       ep.Name,
					API:        ep.API,
					Visibility: ep.Visibility,
				})
			}
			details = append(details, detail)
		}
	}
	return details, nil
}

// DependencyGraph returns services in dependency (startup) order. With an empty
// root it returns the whole workspace's services (set only); with a service
// unique it returns that service's transitive dependencies ordered before it.
func (p *planeImpl) DependencyGraph(ctx context.Context, service string) (DependencyGraph, error) {
	ws, err := p.workspace(ctx)
	if err != nil {
		return DependencyGraph{}, err
	}
	deps, err := architecture.NewServiceDependencies(ctx, ws)
	if err != nil {
		return DependencyGraph{}, fmt.Errorf("build dependency graph: %w", err)
	}
	graph := DependencyGraph{Root: service}
	if service == "" {
		for _, s := range deps.Services() {
			graph.Order = append(graph.Order, s.Unique)
		}
		return graph, nil
	}
	ordered, err := deps.OrderTo(ctx, service)
	if err != nil {
		return DependencyGraph{}, fmt.Errorf("order dependencies to %s: %w", service, err)
	}
	for _, s := range ordered {
		graph.Order = append(graph.Order, s.Unique)
	}
	return graph, nil
}

// FlowStatus reports the state of the active flow. The legacy surface exposes
// only an overall Ready bool (there is no per-service health getter), so the
// per-service list currently carries just the origin service. Richer per-service
// state requires accumulating orchestration StateListener events — a later lift.
func (p *planeImpl) FlowStatus(ctx context.Context, flowID string) (FlowStatus, error) {
	if p.host == nil || p.host.Flows() == nil {
		return FlowStatus{State: FlowIdle}, nil
	}
	var managed engine.ManagedFlow
	if flowID != "" {
		managed = p.host.Flows().Get(flowID)
	} else {
		_, managed = p.host.Flows().Active()
	}
	flow, _ := managed.(*orchestration.Flow)
	if flow == nil {
		if outcome := p.runOutcome(); outcome != nil && (flowID == "" || outcome.flowID == flowID) {
			status := FlowStatus{State: outcome.state}
			if outcome.err != nil {
				status.Error = outcome.err.Error()
			}
			return status, nil
		}
		return FlowStatus{State: FlowIdle}, nil
	}
	state := FlowStarting
	ready := flow.Ready(ctx)
	if ready {
		state = FlowRunning
	}
	status := FlowStatus{State: state}
	if origin := flow.Origin(); origin != nil {
		status.Services = append(status.Services, ServiceStatus{
			Name:    origin.Name,
			State:   state,
			Healthy: ready,
		})
	}
	return status, nil
}
