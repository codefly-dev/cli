package architecture

import (
	"context"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/configurations"
)

/*
Overview builds a dependency graph of the application and its services.
*/

func LoadPublicApplicationGraph(ctx context.Context, project *configurations.Project) ([]*Graph, error) {
	w := wool.Get(ctx).In("LoadApplicationGraph")
	//graph := NewGraph(project.Name)
	var gs []*Graph
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
		g := NewGraph(app.Name)
		g.AddTypedNode(app.Unique(), configurations.APPLICATION)
		// Add one edge for each of the service endpoint
		for _, endpoint := range endpoints {
			service := configurations.ServiceUnique(app.Name, endpoint.Service)
			g.AddTypedNode(service, configurations.SERVICE)
			g.AddEdge(app.Unique(), service)
			e := configurations.FromProtoEndpoint(endpoint)
			g.AddTypedNode(e.Unique(), configurations.ENDPOINT)
			g.AddEdge(service, e.Unique())
		}
		gs = append(gs, g)

	}
	return gs, nil
}
