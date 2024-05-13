package architecture

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
)

/*
Overview builds a dependency graph of the module and its services.
*/

type ServiceDependencies struct {
	Workspace       *resources.Workspace
	uniqueToService map[string]*resources.Service

	Graph *DAG
}

func NewServiceDependencies(ctx context.Context, workspace *resources.Workspace) (*ServiceDependencies, error) {
	w := wool.Get(ctx).In("NewServiceDependencies")

	dep := &ServiceDependencies{
		Workspace:       workspace,
		uniqueToService: make(map[string]*resources.Service),
	}
	err := dep.loadServiceGraph(ctx, workspace)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service graph")
	}
	dep.Graph.verb = "required by"
	return dep, nil
}

func (d *ServiceDependencies) ServiceFromUnique(unique string) (*resources.Service, error) {
	if svc, ok := d.uniqueToService[unique]; ok {
		return svc, nil
	}
	return nil, shared.NewErrorResourceNotFound("service", unique)
}

// DependsOn returns true if the service identified by unique depends on the service identified by other
func (d *ServiceDependencies) DependsOn(unique string, other string) (bool, error) {
	w := wool.Get(context.Background()).In("ServiceDependencies.DependsOn")
	if !d.Graph.HasNode(unique) {
		return false, w.NewError("service <%s> does not exist", unique)
	}
	if !d.Graph.HasNode(other) {
		return false, w.NewError("service <%s> does not exist", other)
	}
	// A depends on B is represented by an path B-> ...->  A
	return d.Graph.ReachableFrom(other, unique), nil
}

type Service struct {
	Unique string
}

type ServiceDependency struct {
	From Service
	To   Service
}

func (d *ServiceDependencies) Print() string {
	return d.Graph.Print()
}

func (d *ServiceDependencies) Services() []Service {
	var out []Service
	for _, node := range d.Graph.Nodes() {
		if node.Type == resources.SERVICE {
			out = append(out, Service{
				Unique: node.ID,
			})
		}
	}
	return out
}

func (d *ServiceDependencies) Dependencies() []ServiceDependency {
	var out []ServiceDependency
	for _, edge := range d.Graph.Edges() {
		out = append(out, ServiceDependency{
			From: Service{
				Unique: edge.From,
			},
			To: Service{
				Unique: edge.To,
			},
		})
	}
	return out
}

// OrderTo returns the list of services "required" to end up with the service identified by unique.
func (d *ServiceDependencies) OrderTo(ctx context.Context, unique string) ([]Service, error) {
	w := wool.Get(ctx).In("OrderTo")
	sub, err := d.Graph.SubGraphTo(unique)
	if err != nil {
		return nil, fmt.Errorf("cannot topologically sort to <%s>: %w", unique, err)
	}
	w.Trace("service dependencies", wool.Field("graph", d.Graph.PrintAsDot()))
	w.Trace("service dependencies", wool.Field("subgraph", sub.PrintAsDot()))
	order, err := sub.TopologicalSortTo(unique)
	if err != nil {
		return nil, fmt.Errorf("cannot topologically sort to <%s>: %w", unique, err)
	}
	w.Trace("service dependencies", wool.Field("order", order))
	var out []Service
	for _, u := range order {
		if u.Type != resources.SERVICE {
			continue
		}
		out = append(out, Service{
			Unique: u.ID,
		})
	}
	return out, nil
}

// DirectRequires returns the list of services that are directly required by the service identified by unique
// Result is sorted by topological order
func (d *ServiceDependencies) DirectRequires(ctx context.Context, unique string) ([]Service, error) {
	w := wool.Get(ctx).In("DirectRequires")
	children, err := d.Graph.SortedParents(unique)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get sorted parents to <%s>", unique)
	}
	var out []Service
	for _, child := range children {
		if child.Type != resources.SERVICE {
			continue
		}
		out = append(out, Service{
			Unique: child.ID,
		})
	}
	return out, nil
}

// DirectDependents returns the list of services that are directly dependent on the service identified by unique
// Result is sorted by topological order
func (d *ServiceDependencies) DirectDependents(ctx context.Context, unique string) ([]Service, error) {
	w := wool.Get(ctx).In("DirectDependents")
	children, err := d.Graph.SortedChildren(unique)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get sorted children from <%s>", unique)
	}
	var out []Service
	for _, child := range children {
		if child.Type != resources.SERVICE {
			continue
		}
		out = append(out, Service{
			Unique: child.ID,
		})
	}
	return out, nil
}

// Restrict restricts the dependencies to the services required by the service identified by unique
func (d *ServiceDependencies) Restrict(ctx context.Context, unique string) (*ServiceDependencies, error) {
	// B is required by A if A <- ... <- B
	sub, err := d.Graph.SubGraphTo(unique)
	if err != nil {
		return nil, fmt.Errorf("cannot restrict to <%s>: %w", unique, err)
	}
	return &ServiceDependencies{
		Workspace: d.Workspace,
		Graph:     sub,
	}, nil
}

// X depends on Y means an edge X <- Y
func (d *ServiceDependencies) loadServiceGraph(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("loadServiceGraph")
	w.Debug("analyzing workspace", wool.Field("workspace", workspace.Name), wool.Field("#modules", len(workspace.Modules)))
	graph := NewDAG(workspace.Name)
	for _, modRef := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, modRef)
		if err != nil {
			return w.Wrapf(err, "cannot load module <%s>", modRef.Name)
		}
		for _, serviceRef := range mod.ServiceReferences {
			svc, err := mod.LoadServiceFromReference(ctx, serviceRef)
			if err != nil {
				return w.Wrapf(err, "cannot load svc <%s>", serviceRef.Name)
			}
			d.uniqueToService[svc.Unique()] = svc
			graph.AddNode(svc.Unique()).WithType(resources.SERVICE)
			for _, dep := range svc.ServiceDependencies {
				graph.AddNode(dep.Unique()).WithType(resources.SERVICE)
				graph.AddEdge(dep.Unique(), svc.Unique())
			}
		}
	}
	d.Graph = graph
	return nil
}
