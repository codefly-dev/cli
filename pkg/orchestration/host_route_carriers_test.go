package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/remotenetwork"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The host gateway forwards a composed solution's call to a module it consumes
// (/v1/<as>/…), and the Codefly SDK in the gateway resolves that module from
// the carriers the composition writes. These pin what the composition writes.

// hostRoutesWorld is a world over a copy of testdata/host-routes whose
// environment names host/gateway as the host's gateway. edit, when set, may
// change the copy before it is loaded.
func hostRoutesWorld(t *testing.T, mode Mode, edit func(dir string)) (*World, *resources.Workspace) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "host-routes")
	require.NoError(t, os.CopyFS(dir, os.DirFS("testdata/host-routes")))
	if edit != nil {
		edit(dir)
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	env.Host = &environments.EnvironmentHost{Gateway: "host/gateway"}
	manager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	require.NoError(t, manager.Load(ctx, env.Runtime()))
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	sharedState, err := NewStateManager(ctx, manager, dependencies, workspace)
	require.NoError(t, err)
	localNetwork, err := network.NewRuntimeManager(ctx, manager)
	require.NoError(t, err)
	remoteNetwork, err := remotenetwork.NewRemoteManager(ctx, manager)
	require.NoError(t, err)
	return &World{
		Mode: mode, Env: env, Workspace: workspace,
		ConfigurationManager: manager, SharedState: sharedState, Dependencies: dependencies,
		LocalNetworkManager: localNetwork, RemoteNetworkManager: remoteNetwork,
		runtimeContextFor: func(*resources.Service) string { return resources.RuntimeContextNative },
	}, workspace
}

func writeWikiConsumes(t *testing.T, dir, consumes string) {
	t.Helper()
	path := filepath.Join(dir, "modules", "wiki", "solution.codefly.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	head, _, found := strings.Cut(string(raw), "  consumes:\n")
	require.True(t, found)
	_, tail, found := strings.Cut(string(raw), "lifecycle:")
	require.True(t, found)
	require.NoError(t, os.WriteFile(path, []byte(head+"  consumes:\n"+consumes+"lifecycle:"+tail), 0o600))
}

// In a render the gateway gets the in-cluster carrier of what the wiki consumes
// through the host — and nothing for the host's own endpoint. The address is
// the producer's as its own deploy records it, derived from ALL its endpoints:
// web (http) shares no port with rest, which a derivation over the consumed
// endpoint alone would get wrong.
func TestTheHostGatewayIsHandedTheAddressOfWhatSolutionsConsume(t *testing.T) {
	ctx := context.Background()
	world, workspace := hostRoutesWorld(t, SnapshotMode, nil)
	gateway := parityService(t, workspace, "host", "gateway")

	carriers, problems, err := world.hostRouteCarriers(ctx, gateway, resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	require.Empty(t, problems)
	key := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "docs", Service: "documents", Name: "web", API: "http"})
	backend := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "wiki", Service: "backend", Name: "http", API: "http"})
	require.Len(t, carriers, 2, "the host's own endpoint is not a route: %v", carriers)
	require.Contains(t, carriers, key)
	require.Contains(t, carriers, backend, "the solution's backend is routed like a consumed module")
	require.Contains(t, carriers[backend], "wiki", "the solution's own in-cluster address: %s", carriers[backend])
	require.NotContains(t, carriers[backend], "https://", "plain HTTP: a solution serves no certificate, the mesh carries its identity")

	producer, err := world.producerNetworkMappings(ctx, "docs/documents")
	require.NoError(t, err)
	var web, rest string
	for _, mapping := range producer {
		for _, instance := range mapping.GetInstances() {
			if instance.GetAccess().GetKind() != resources.NewContainerNetworkAccess().GetKind() {
				continue
			}
			switch mapping.GetEndpoint().GetName() {
			case "web":
				web = instance.GetAddress()
			case "rest":
				rest = instance.GetAddress()
			}
		}
	}
	require.NotEmpty(t, web)
	require.Equal(t, web, carriers[key], "the carrier is the address the producer's own deploy records")
	require.NotEqual(t, rest, web, "web is not rest's port")
	require.Contains(t, web, ".svc.cluster.local", "an in-cluster address")

	other := parityService(t, workspace, "wiki", "backend")
	none, problems, err := world.hostRouteCarriers(ctx, other, resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	require.Empty(t, problems)
	require.Nil(t, none, "only the host gateway forwards consumed calls")
}

func TestTheHostGatewaysRoutesAreRefusedWhenTheyHaveNoSingleAnswer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		consumes string
		extra    func(dir string)
		want     string
	}{
		{
			name:     "consumed under another alias",
			consumes: "    - id: docs\n      protocol: http\n      module: docs\n      service: documents\n      endpoint: web\n      as: documents\n",
			want:     `must be consumed as "docs"`,
		},
		{
			name:     "an endpoint the gateway's module may not reach",
			consumes: "    - id: docs\n      protocol: http\n      module: docs\n      service: documents\n      endpoint: admin\n      as: docs\n",
			want:     "visibility does not admit",
		},
		{
			name:     "a module the workspace does not compose",
			consumes: "    - id: absent\n      protocol: rest\n      module: absent\n      service: api\n      endpoint: rest\n      as: absent\n",
			want:     "does not compose",
		},
		{
			name:     "two solutions reaching one module through different endpoints",
			consumes: "    - id: docs\n      protocol: http\n      module: docs\n      service: documents\n      endpoint: web\n      as: docs\n",
			extra: func(dir string) {
				second := filepath.Join(dir, "modules", "notes")
				require.NoError(t, os.CopyFS(second, os.DirFS(filepath.Join(dir, "modules", "wiki"))))
				for _, file := range []string{"module.codefly.yaml", "services/backend/service.codefly.yaml", "solution.codefly.yaml"} {
					path := filepath.Join(second, file)
					raw, err := os.ReadFile(path)
					require.NoError(t, err)
					text := strings.ReplaceAll(string(raw), "wiki", "notes")
					text = strings.ReplaceAll(text, "endpoint: web", "endpoint: rest")
					text = strings.ReplaceAll(text, "protocol: http\n      module: docs", "protocol: rest\n      module: docs")
					require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
				}
				workspace := filepath.Join(dir, "workspace.codefly.yaml")
				raw, err := os.ReadFile(workspace)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(workspace, append(raw, []byte("    - name: notes\n")...), 0o600))
			},
			want: "through different endpoints",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world, workspace := hostRoutesWorld(t, SnapshotMode, func(dir string) {
				writeWikiConsumes(t, dir, tc.consumes)
				if tc.extra != nil {
					tc.extra(dir)
				}
			})
			gateway := parityService(t, workspace, "host", "gateway")
			_, problems, err := world.hostRouteCarriers(context.Background(), gateway, resources.NewContainerNetworkAccess())
			if err == nil {
				err = errors.Join(problems...)
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A local run hands the gateway the address this run allocated for the
// consumed endpoint, at the gateway's own network access; a producer the run
// has not addressed yet is a warning, never a failed start.
func TestALocalRunHandsTheGatewayTheRunsAddress(t *testing.T) {
	ctx := context.Background()
	world, workspace := hostRoutesWorld(t, RunMode, nil)
	gateway := parityService(t, workspace, "host", "gateway")
	access := resources.NetworkAccessFromRuntimeContext(&basev0.RuntimeContext{Kind: resources.RuntimeContextNative})

	none, problems, err := world.hostRouteCarriers(ctx, gateway, access)
	require.NoError(t, err, "a producer this run has not addressed is not a fault of the composition")
	require.Nil(t, none)
	require.Len(t, problems, 2, "neither the consumed module nor the solution's backend is addressed yet")
	require.Contains(t, problems[0].Error(), "has not allocated")
	require.Contains(t, problems[1].Error(), "the host fronts component wiki at its backend endpoint wiki/backend/http")

	for _, unique := range []string{"docs/documents", "wiki/backend"} {
		producer := parityService(t, workspace, strings.Split(unique, "/")[0], strings.Split(unique, "/")[1])
		identity, err := producer.Identity()
		require.NoError(t, err)
		endpoints, err := producer.LoadEndpoints(ctx)
		require.NoError(t, err)
		require.NoError(t, world.SharedState.RecordEndpoints(ctx, identity, endpoints))
	}

	carriers, problems, err := world.hostRouteCarriers(ctx, gateway, access)
	require.NoError(t, err)
	require.Empty(t, problems)
	key := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "docs", Service: "documents", Name: "web", API: "http"})
	require.Contains(t, carriers, key)
	backend := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "wiki", Service: "backend", Name: "http", API: "http"})
	require.Contains(t, carriers, backend, "the solution's backend, at the run's address")
	require.Contains(t, carriers[backend], "localhost")
	// The run's own address for the producer's web endpoint, derived over all
	// its endpoints as its Init proposes it — not rest's port.
	mappings, err := world.producerNetworkMappings(ctx, "docs/documents")
	require.NoError(t, err)
	manager := resources.NewEnvironmentVariableManager()
	require.NoError(t, manager.AddEndpoints(ctx, mappings, access))
	var want, rest string
	for _, endpoint := range manager.Endpoints() {
		switch endpoint.Endpoint.GetName() {
		case "web":
			want = endpoint.NetworkInstance.GetAddress()
		case "rest":
			rest = endpoint.NetworkInstance.GetAddress()
		}
	}
	require.Equal(t, want, carriers[key])
	require.NotEqual(t, rest, carriers[key])
	require.Contains(t, carriers[key], "localhost", "a local address, at the gateway's own access")
}

// One route that has no answer never takes another route's carrier with it: a
// run warns about it and the gateway starts with every other route.
func TestOneUnroutableConsumeLeavesTheOtherRoutes(t *testing.T) {
	ctx := context.Background()
	world, workspace := hostRoutesWorld(t, SnapshotMode, func(dir string) {
		writeWikiConsumes(t, dir,
			"    - id: docs\n      protocol: http\n      module: docs\n      service: documents\n      endpoint: web\n      as: docs\n"+
				"    - id: archive\n      protocol: rest\n      module: archive\n      service: vault\n      endpoint: rest\n      as: archive\n")
		module := filepath.Join(dir, "modules", "archive")
		require.NoError(t, os.MkdirAll(filepath.Join(module, "services", "vault"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(module, "module.codefly.yaml"), []byte("kind: module\nname: archive\nservices:\n    - name: vault\n"), 0o600))
		// Private: the gateway's module may not reach it.
		require.NoError(t, os.WriteFile(filepath.Join(module, "services", "vault", "service.codefly.yaml"), []byte(
			"kind: service\nname: vault\nversion: 0.0.0\nmodule: archive\nagent:\n    kind: runtime::service\n    name: go\n    version: 0.0.1\n    publisher: codefly.dev\nendpoints:\n    - name: rest\n      api: rest\n"), 0o600))
		raw, err := os.ReadFile(filepath.Join(dir, "workspace.codefly.yaml"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), append(raw, []byte("    - name: archive\n")...), 0o600))
	})
	gateway := parityService(t, workspace, "host", "gateway")
	carriers, problems, err := world.hostRouteCarriers(ctx, gateway, resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	require.Len(t, problems, 1)
	require.Contains(t, problems[0].Error(), "archive/vault/rest")
	require.Contains(t, carriers, resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "docs", Service: "documents", Name: "web", API: "http"}))
}

// A composed solution's backend is one HTTP endpoint of its entry service: an
// entry that declares none, or several, leaves the host with no single
// endpoint to forward /<alias>/ to, and the composition is refused naming it
// rather than one being chosen.
func TestASolutionBackendIsOneHTTPEndpointOfItsEntry(t *testing.T) {
	for _, tc := range []struct{ name, endpoints, want string }{
		{"none", "endpoints:\n    - name: grpc\n      api: grpc\n", "declares no http or rest endpoint"},
		{"several", "endpoints:\n    - name: http\n      api: http\n    - name: rest\n      api: rest\n", "several http or rest endpoints (http, rest)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world, workspace := hostRoutesWorld(t, SnapshotMode, func(dir string) {
				path := filepath.Join(dir, "modules", "wiki", "services", "backend", "service.codefly.yaml")
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				head, _, found := strings.Cut(string(raw), "endpoints:\n")
				require.True(t, found)
				require.NoError(t, os.WriteFile(path, []byte(head+tc.endpoints), 0o600))
			})
			gateway := parityService(t, workspace, "host", "gateway")
			_, _, err := world.hostRouteCarriers(context.Background(), gateway, resources.NewContainerNetworkAccess())
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "wiki")
		})
	}
}

// A shared module's page is a route independently of any solution's API needs.
func TestModulePageGetsACarrierWithoutASolutionConsumer(t *testing.T) {
	for _, mode := range []Mode{SnapshotMode, RunMode} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			world, workspace := hostRoutesWorld(t, mode, func(dir string) {
				writeWikiConsumes(t, dir, "")
				module := filepath.Join(dir, "modules", "docs")
				path := filepath.Join(module, "module.codefly.yaml")
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, append(raw, []byte("service-entry: documents\n")...), 0o600))
				entry := filepath.Join(module, "services", "documents")
				require.NoError(t, os.WriteFile(filepath.Join(entry, "solution.frontend.codefly.yaml"), []byte("title: Documents\norder: 0\nexposed_module: ./Page\nhost_contract: 1\n"), 0o600))
				path = filepath.Join(entry, "service.codefly.yaml")
				raw, err = os.ReadFile(path)
				require.NoError(t, err)
				head, _, found := strings.Cut(string(raw), "endpoints:\n")
				require.True(t, found)
				require.NoError(t, os.WriteFile(path, []byte(head+"endpoints:\n    - name: web\n      api: http\n      visibility: internal\n"), 0o600))
			})
			gateway := parityService(t, workspace, "host", "gateway")
			access := resources.NewContainerNetworkAccess()
			if mode == RunMode {
				access = resources.NetworkAccessFromRuntimeContext(&basev0.RuntimeContext{Kind: resources.RuntimeContextNative})
				for _, pair := range [][2]string{{"docs", "documents"}, {"wiki", "backend"}} {
					service := parityService(t, workspace, pair[0], pair[1])
					identity, err := service.Identity()
					require.NoError(t, err)
					endpoints, err := service.LoadEndpoints(t.Context())
					require.NoError(t, err)
					require.NoError(t, world.SharedState.RecordEndpoints(t.Context(), identity, endpoints))
				}
			}
			carriers, problems, err := world.hostRouteCarriers(t.Context(), gateway, access)
			require.NoError(t, err)
			require.Empty(t, problems)
			key := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "docs", Service: "documents", Name: "web", API: "http"})
			require.NotEmpty(t, carriers[key], "the module page needs its own route without a solution consumer")
		})
	}
}

// A local run tells an endpoint the producer does not declare from one it has
// not addressed yet: the first is named as missing, never as waiting on an
// address no allocation will ever give it.
func TestALocalRunNamesAnUndeclaredEndpointAsMissing(t *testing.T) {
	consumes := "    - id: docs\n      protocol: connect\n      module: docs\n      service: documents\n      endpoint: connect\n      as: docs\n"
	world, workspace := hostRoutesWorld(t, RunMode, func(dir string) { writeWikiConsumes(t, dir, consumes) })
	gateway := parityService(t, workspace, "host", "gateway")
	access := resources.NetworkAccessFromRuntimeContext(&basev0.RuntimeContext{Kind: resources.RuntimeContextNative})

	_, problems, err := world.hostRouteCarriers(context.Background(), gateway, access)
	require.NoError(t, err)
	require.NotEmpty(t, problems)
	require.Contains(t, problems[0].Error(), `docs/documents declares no endpoint named "connect"`)
	require.NotContains(t, problems[0].Error(), "has not allocated")
}
