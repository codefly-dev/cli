package control

import (
	"context"
	"fmt"
	"sort"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
)

// planeImpl is the single implementation of Plane. It caches nothing beyond
// construction: like the legacy pkg/web server it (re)loads the workspace from
// the current directory on demand, and reaches a running flow through the
// orchestration.CurrentFlow() global. Capability groups are lifted onto it one
// at a time; methods not yet lifted live in unimplemented.go and return
// errNotImplemented.
type planeImpl struct {
	// gate enforces the mutation-authority policy for destructive/outward
	// actions (Deploy and, under prepared authority, edits). See mutation.go.
	gate *mutationGate
}

// New returns the control plane. Most operations resolve the workspace/flow at
// call time; the only construction-time state is the mutation-authority gate,
// which defaults to open (trusted local caller).
func New() Plane {
	return &planeImpl{gate: newMutationGate()}
}

// Compile-time proof that planeImpl satisfies the full control surface.
var _ Plane = (*planeImpl)(nil)

// workspace loads the workspace rooted at the current directory. Kept private so
// every method resolves it the same way.
func (p *planeImpl) workspace(ctx context.Context) (*resources.Workspace, error) {
	ws, err := resources.FindWorkspaceUp(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	if ws == nil {
		return nil, fmt.Errorf("no workspace found from the current directory")
	}
	return ws, nil
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
	flow := orchestration.CurrentFlow()
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
