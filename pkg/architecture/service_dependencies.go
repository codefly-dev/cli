package architecture

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/configurations"
)

/*
Overview builds a dependency graph of the application and its services.
*/

type ServiceDependencies struct {
	Project *configurations.Project
	Graph   *DAG
}

func NewServiceDependencies(ctx context.Context, project *configurations.Project) (*ServiceDependencies, error) {
	w := wool.Get(ctx).In("NewServiceDependencies")
	g, err := loadServiceGraph(ctx, project)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service graph")
	}
	g.verb = "required by"
	return &ServiceDependencies{
		Project: project,
		Graph:   g,
	}, nil
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
		if node.Type == configurations.SERVICE {
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
	sub, err := d.Graph.SubGraphTo(unique)
	if err != nil {
		return nil, fmt.Errorf("cannot topologically sort to <%s>: %w", unique, err)
	}
	order, err := sub.TopologicalSortTo(unique)
	if err != nil {
		return nil, fmt.Errorf("cannot topologically sort to <%s>: %w", unique, err)
	}
	var out []Service
	for _, u := range order {
		if u.Type != configurations.SERVICE {
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
		if child.Type != configurations.SERVICE {
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
		if child.Type != configurations.SERVICE {
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
		Project: d.Project,
		Graph:   sub,
	}, nil
}

// X depends on Y means an edge X <- Y
func loadServiceGraph(ctx context.Context, project *configurations.Project) (*DAG, error) {
	w := wool.Get(ctx).In("LoadServiceGraph")
	graph := NewDAG(project.Name)
	for _, appRef := range project.Applications {
		app, err := project.LoadApplicationFromReference(ctx, appRef)
		if err != nil {
			return nil, w.Wrapf(err, "cannot load application <%s>", appRef.Name)
		}
		for _, serviceRef := range app.ServiceReferences {
			service, err := app.LoadServiceFromReference(ctx, serviceRef)
			if err != nil {
				return nil, w.Wrapf(err, "cannot load service <%s>", serviceRef.Name)
			}
			graph.AddNode(service.Unique()).WithType(configurations.SERVICE)
			for _, dep := range service.ServiceDependencies {
				graph.AddNode(dep.Unique()).WithType(configurations.SERVICE)
				graph.AddEdge(dep.Unique(), service.Unique())
			}
		}
	}
	return graph, nil
}
