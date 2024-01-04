package architecture

import (
	"context"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/configurations"
)

/*
Overview builds a dependency graph of the application and its services.
*/

func LoadServiceGraph(ctx context.Context, project *configurations.Project) (*Graph, error) {
	w := wool.Get(ctx).In("LoadServiceGraph")
	graph := NewGraph(project.Name)
	for _, appRef := range project.Applications {
		app, err := project.LoadApplicationFromReference(ctx, appRef)
		if err != nil {
			return nil, w.Wrapf(err, "cannot load application <%s>", appRef.Name)
		}
		for _, serviceRef := range app.Services {
			service, err := app.LoadServiceFromReference(ctx, serviceRef)
			if err != nil {
				return nil, w.Wrapf(err, "cannot load service <%s>", serviceRef.Name)
			}
			graph.AddTypedNode(service.Unique(), configurations.SERVICE)
			for _, dep := range service.Dependencies {
				graph.AddTypedNode(dep.Unique(), configurations.SERVICE)
				graph.AddEdge(dep.Unique(), service.Unique())
			}
		}
	}
	return graph, nil
}
