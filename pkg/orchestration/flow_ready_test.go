package orchestration

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/codefly-dev/core/architecture"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestFlowReadyExcludeRootWaitsForDependenciesOnly(t *testing.T) {
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/module-layout")
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "web")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "frontend")
	require.NoError(t, err)
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)

	flow := &Flow{
		originService: service,
		world:         &World{Dependencies: dependencies},
		playbook:      &Playbook{},
		excludeRoot:   true,
	}
	require.False(t, flow.Ready(ctx))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	flow.SharedState = &StateManager{
		networkMappings: map[string][]*basev0.NetworkMapping{
			"web/gateway": {
				{
					Endpoint:  &basev0.Endpoint{Name: "rest", Module: "web", Service: "gateway"},
					Instances: []*basev0.NetworkInstance{nativeTestInstance(listener.Addr().String())},
				},
			},
		},
	}
	require.True(t, flow.Ready(ctx))

	flow.playbook.executed = nil
	flow.excludeRoot = false
	require.False(t, flow.Ready(ctx))

	flow.playbook.executed = []Action{{Type: RuntimeStart, Service: "web/frontend"}}
	require.True(t, flow.Ready(ctx))
}

func TestFormatServiceRunPlanEntryIncludesServiceAndAgentVersions(t *testing.T) {
	service := &resources.Service{
		Name:    "api",
		Version: "0.0.7",
		Agent: &resources.Agent{
			Kind:      resources.ServiceAgent,
			Publisher: "codefly.dev",
			Name:      "go-grpc",
			Version:   "latest",
		},
	}

	got := formatServiceRunPlanEntry("users/api", service, nil)
	require.Equal(t, "users/api@0.0.7 via codefly.dev/go-grpc:latest", got)
}

func TestNetworkMappingsReachableRequiresHTTPForBoltHTTPServices(t *testing.T) {
	bolt, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer bolt.Close()

	mappings := []*basev0.NetworkMapping{
		{
			Endpoint:  &basev0.Endpoint{Name: "bolt"},
			Instances: []*basev0.NetworkInstance{nativeTestInstance(bolt.Addr().String())},
		},
		{
			Endpoint:  &basev0.Endpoint{Name: "http"},
			Instances: []*basev0.NetworkInstance{nativeTestInstance("127.0.0.1:1")},
		},
	}
	require.False(t, networkMappingsReachable(mappings))

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer httpServer.Close()
	parsed, err := url.Parse(httpServer.URL)
	require.NoError(t, err)
	mappings[1].Instances[0].Host = parsed.Host
	require.True(t, networkMappingsReachable(mappings))
}

func nativeTestInstance(host string) *basev0.NetworkInstance {
	return &basev0.NetworkInstance{
		Access: &basev0.NetworkAccess{Kind: resources.NetworkAccessNative},
		Host:   host,
	}
}
