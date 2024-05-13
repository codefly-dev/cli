package architecture

import (
	"context"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func LoadPublicModuleGraph(ctx context.Context, workspace *resources.Workspace) ([]*DAG, error) {
	w := wool.Get(ctx).In("LoadModuleGraph")
	var gs []*DAG
	for _, modRef := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, modRef)
		if err != nil {
			return nil, w.With(wool.NameField(modRef.Name)).Wrapf(err, "cannot load module")
		}
		endpoints, err := mod.PublicEndpoints(ctx)
		if err != nil {
			return nil, w.With(wool.NameField(modRef.Name)).Wrapf(err, "cannot load public endpoints")
		}
		if len(endpoints) == 0 {
			continue
		}
		g := NewDAG(mod.Name)
		g.AddNode(mod.Unique()).WithType(resources.MODULE)
		// Add one edge for each of the service endpoint
		for _, endpoint := range endpoints {
			service := resources.ServiceUnique(mod.Name, endpoint.Service)
			g.AddNode(service).WithType(resources.SERVICE)
			g.AddEdge(mod.Unique(), service)
			e := resources.EndpointFromProto(endpoint)
			g.AddNode(e.Unique()).WithType(resources.ENDPOINT)
			g.AddEdge(service, e.Unique())
		}
		gs = append(gs, g)

	}
	return gs, nil
}
