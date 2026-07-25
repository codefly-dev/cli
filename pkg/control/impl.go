package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
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
func (p *planeImpl) FlowStatus(ctx context.Context) (FlowStatus, error) {
	if p.host == nil || p.host.Flows() == nil {
		return FlowStatus{State: FlowIdle}, nil
	}
	_, managed := p.host.Flows().Active()
	flow, _ := managed.(*orchestration.Flow)
	if flow == nil {
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
