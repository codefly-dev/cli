package orchestration

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corenetwork "github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Only an endpoint with a container-access instance has an in-cluster port:
// the external endpoint that resolves to a public host is absent.
func TestInClusterPortsReadsContainerAccessInstances(t *testing.T) {
	internal := &basev0.Endpoint{Name: "grpc", Api: "grpc", Module: "shop", Service: "api"}
	// External is a LOCATION now, beside whatever visibility applies: it says
	// where the endpoint lives, not who may reach it. (core v0.11.0.)
	external := &basev0.Endpoint{Name: "http", Api: "http", Module: "shop", Service: "api",
		Visibility: resources.VisibilityPublic, Location: resources.LocationExternal}
	public := resources.NewNetworkInstance("api.shop.svc.cluster.local", 19043)
	container := resources.NewNetworkInstance("api.shop.svc.cluster.local", 19043)
	mappings := []*basev0.NetworkMapping{
		{Endpoint: internal, Instances: []*basev0.NetworkInstance{corenetwork.PublicInstance(public), corenetwork.ContainerInstance(container)}},
		{Endpoint: external, Instances: []*basev0.NetworkInstance{corenetwork.ExternalInstance(
			resources.NewHTTPNetworkInstance("app.example.com", 443, true),
		)}},
	}
	require.Equal(t, map[string]uint32{"grpc": 19043}, InClusterPorts(context.Background(), mappings))
	require.Empty(t, (*Flow)(nil).InClusterPorts(context.Background()))
}
