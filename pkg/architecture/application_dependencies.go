package architecture

import (
	"context"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/configurations"
)

/*
Overview builds a dependency graph of the application and its services.
*/

func LoadPublicApplicationGraph(ctx context.Context, project *configurations.Project) ([]*DAG, error) {
	w := wool.Get(ctx).In("LoadApplicationGraph")
	//graph := NewDAG(project.Name)
	var gs []*DAG
	for _, appRef := range project.Applications {
		app, err := project.LoadApplicationFromReference(ctx, appRef)
		if err != nil {
			return nil, w.With(wool.NameField(appRef.Name)).Wrapf(err, "cannot load application")
		}
		endpoints, err := app.PublicEndpoints(ctx)
		if err != nil {
			return nil, w.With(wool.NameField(appRef.Name)).Wrapf(err, "cannot load public endpoints")
		}
		if len(endpoints) == 0 {
			continue
		}
		g := NewDAG(app.Name)
		g.AddNode(app.Unique()).WithType(configurations.APPLICATION)
		// Add one edge for each of the service endpoint
		for _, endpoint := range endpoints {
			service := configurations.ServiceUnique(app.Name, endpoint.Service)
			g.AddNode(service).WithType(configurations.SERVICE)
			g.AddEdge(app.Unique(), service)
			e := configurations.FromProtoEndpoint(endpoint)
			g.AddNode(e.Unique()).WithType(configurations.ENDPOINT)
			g.AddEdge(service, e.Unique())
		}
		gs = append(gs, g)

	}
	return gs, nil
}
