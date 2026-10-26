package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/authority/libraries/hop/go/routing"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func hopRouteWorkspace(t *testing.T, visibility string) *resources.Workspace {
	t.Helper()
	files := map[string]string{
		"workspace.codefly.yaml":                                "name: routes\nlayout: modules\nmodules:\n - name: platform\n - name: payments\n",
		"configurations/local/hop.env":                          "MODULES=platform\n",
		"modules/platform/module.codefly.yaml":                  "name: platform\nservices:\n - name: first\n - name: second\n",
		"modules/platform/module.contract.codefly.yaml":         "schema: codefly/module-contract/v1\nprincipal: platform\nnamespaces: [platform]\nqueues: []\nscope_ceilings: []\nbindings: []\n",
		"modules/payments/module.codefly.yaml":                  "name: payments\nservices:\n - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "name: worker\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nworkspace-configuration-dependencies: [hop-routes]\n",
	}
	catalog := composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "owner/platform", Version: "1.0.0"}
	manifest := "schema: codefly/module-package/v2\nkind: module-package\nid: owner/platform\nversion: 1.0.0\nminimum-codefly-version: '>=0.3.32 <1.0.0'\nartifact-roots: [contracts, services]\ncontracts: {composition: '>=2.0.0 <3.0.0'}\nservices:\n"
	for _, service := range []string{"first", "second"} {
		path := "contracts/api/" + service + ".json"
		doc := fmt.Sprintf(`{"openapi":"3.0.0","info":{"title":"%s","version":"1.0.0"},"paths":{"/%s":{"get":{"responses":{"200":{"description":"ok"}}}}}}`, service, service)
		digest := composition.APIContractDigest([]byte(doc))
		files["modules/platform/"+path] = doc
		files["modules/platform/services/"+service+"/service.codefly.yaml"] = "name: " + service + "\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nendpoints:\n    - name: web\n      api: http\n      visibility: " + visibility + "\n" + statedExposure(visibility)
		manifest += fmt.Sprintf(" - name: %s\n   endpoints: [web]\n   api-contracts:\n    - endpoint: web\n      kind: openapi\n      package: api\n      path: %s\n      digest: %s\n", service, path, digest)
		catalog.Endpoints = append(catalog.Endpoints, composition.APIContractEndpoint{Service: service, Endpoint: "web", API: "http", Kind: composition.APIContractKindOpenAPI, Package: "api", Path: path, Digest: digest, Routes: []composition.APIContractRoute{{Method: "GET", Path: "/" + service}}})
	}
	raw, err := json.Marshal(catalog)
	require.NoError(t, err)
	files["modules/platform/"+composition.APIContractCatalogFileName] = string(raw)
	files["modules/platform/module.package.codefly.yaml"] = manifest
	return writeTempWorkspace(t, files)
}

func TestGeneratedHopRoutesUseTheNormalGraphAndAddressResolver(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	workspace := hopRouteWorkspace(t, "public")
	flow := declaredReferenceFlow(t, workspace)
	require.NoError(t, flow.checkConfigurationReferences(ctx, nil))
	deps, err := flow.managerDependencies(ctx)
	require.NoError(t, err)
	var uniques []string
	for _, dep := range deps {
		uniques = append(uniques, dep.Unique)
	}
	require.Contains(t, uniques, "platform/first")
	require.Contains(t, uniques, "platform/second")
	world := flow.world
	world.setRunProducers(uniques, flow.originService)
	require.NoError(t, flow.ConfigurationManager.Load(ctx, world.Env.Runtime()))
	require.NotContains(t, world.compositionRootGroups(), hopRoutesGroup, "generated routes must not reach every workload")
	for _, service := range []string{"first", "second"} {
		recordParityEndpoints(t, world, "platform", service)
		recordMappings(t, world, "platform", service, endpointMapping("platform", service, "web", "http", "public", nativeInstance("http://"+service+".native"), containerInstance("http://"+service+".cell")))
	}
	for _, tc := range []struct {
		access *basev0.NetworkAccess
		suffix string
		mode   Mode
	}{{resources.NewNativeNetworkAccess(), "native", RunMode}, {resources.NewContainerNetworkAccess(), "cell", SnapshotMode}} {
		world.Mode = tc.mode
		configs, err := world.workspaceConfigurationsFor(ctx, flow.originService, nil, tc.access)
		require.NoError(t, err)
		raw, delivered := groupValue(configs, hopRoutesGroup, "ROUTES")
		require.True(t, delivered)
		routes, err := routing.DecodeRouting(raw)
		require.NoError(t, err)
		for _, service := range []string{"first", "second"} {
			route, err := routes.Find("platform", "http", "GET /"+service)
			require.NoError(t, err)
			require.Equal(t, service, route.Service)
			require.Equal(t, "http://"+service+"."+tc.suffix, route.Address)
		}
	}
	service, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	first, err := service.LoadServiceFromName(ctx, "first")
	require.NoError(t, err)
	moved := endpointMapping("platform", "first", "web", "http", "public", nativeInstance("http://moved.native"), containerInstance("http://first.cell"))
	err = world.SharedState.RecordNetworkMappings(ctx, first, []*basev0.NetworkMapping{moved})
	require.ErrorContains(t, err, "stop and restart")
	previous, found := world.SharedState.GetNetworkMappingsFromUnique("platform/first")
	require.True(t, found)
	require.NotContains(t, fmt.Sprint(previous), "moved.native", "rejected mapping never replaces accepted route")
}

// A fresh run initializes the hop before a routed module that is ordered after
// it (a module that calls through the hop is). That module has loaded, so its
// address is the one its own Init will be proposed; the hop's snapshot freezes
// it, and the module's Init is then the check: the same address is accepted,
// any other is refused naming the route. Before, the hop's own start was
// refused as a routing change although nothing had changed.
func TestHopRoutesFreezeAModuleThatInitializesAfterTheHop(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	workspace := hopRouteWorkspace(t, "public")
	flow := declaredReferenceFlow(t, workspace)
	require.NoError(t, flow.checkConfigurationReferences(ctx, nil))
	deps, err := flow.managerDependencies(ctx)
	require.NoError(t, err)
	var uniques []string
	for _, dep := range deps {
		uniques = append(uniques, dep.Unique)
	}
	world := flow.world
	world.setRunProducers(uniques, flow.originService)
	require.NoError(t, flow.ConfigurationManager.Load(ctx, world.Env.Runtime()))
	world.Mode = RunMode
	world.runtimeContextFor = func(*resources.Service) string { return resources.RuntimeContextNative }
	// Both loaded; only first has initialized.
	recordParityEndpoints(t, world, "platform", "first")
	recordParityEndpoints(t, world, "platform", "second")
	recordMappings(t, world, "platform", "first", endpointMapping("platform", "first", "web", "http", "public", nativeInstance("http://first.native"), containerInstance("http://first.cell")))
	proposed, err := world.producerNetworkMappings(ctx, "platform/second")
	require.NoError(t, err)
	require.Len(t, proposed, 1)

	configs, err := world.workspaceConfigurationsFor(ctx, flow.originService, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err, "a fresh run never sees changed routes")
	raw, delivered := groupValue(configs, hopRoutesGroup, "ROUTES")
	require.True(t, delivered)
	routes, err := routing.DecodeRouting(raw)
	require.NoError(t, err)
	route, err := routes.Find("platform", "http", "GET /second")
	require.NoError(t, err)
	native, err := resources.FindNetworkInstanceInNetworkMappings(ctx, proposed, proposed[0].GetEndpoint(), resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.Equal(t, native.GetAddress(), route.Address, "the hop routes second at the address its Init will be proposed")

	module, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	second, err := module.LoadServiceFromName(ctx, "second")
	require.NoError(t, err)
	// An Init that accepts another address than the frozen one is refused by
	// name, and nothing is recorded in its place.
	moved := endpointMapping("platform", "second", "web", "http", "public", nativeInstance("http://moved.native"))
	err = world.SharedState.RecordNetworkMappings(ctx, second, []*basev0.NetworkMapping{moved})
	require.ErrorContains(t, err, "hop route platform/second/web::http is routed at "+mappingAddresses(proposed...))
	require.ErrorContains(t, err, "its Init accepted [native http://moved.native]")
	// The address is derived, so a restart cannot help and is not asked for.
	require.ErrorContains(t, err, "restarting this run derives it again")
	require.NotContains(t, err.Error(), "stop and restart")
	_, recorded := world.SharedState.GetNetworkMappingsFromUnique("platform/second")
	require.False(t, recorded, "a refused Init records nothing")
	// An Init that drops the routed endpoint is refused the same way.
	require.ErrorContains(t, world.SharedState.RecordNetworkMappings(ctx, second, nil), "its Init accepted no address for it")
	// The address the hop froze is accepted.
	require.NoError(t, world.SharedState.RecordNetworkMappings(ctx, second, proposed))
	// And the hop's next delivery reads the same snapshot.
	_, err = world.workspaceConfigurationsFor(ctx, flow.originService, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
}

// hopFenceState is a run's shared state over hopRouteWorkspace with
// platform/second loaded but not initialized, and one hop route to its web
// listener.
func hopFenceState(t *testing.T) (*StateManager, *resources.Service, string) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	workspace := hopRouteWorkspace(t, "public")
	state, err := NewStateManager(ctx, nil, nil, workspace)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	second, err := module.LoadServiceFromName(ctx, "second")
	require.NoError(t, err)
	raw, err := json.Marshal(routing.Routing{Schema: routing.RoutingSchema, Endpoints: []routing.RouteEndpoint{{
		Audience: "platform", Service: "second", Endpoint: "web", Transport: "http", Address: "http://derived.native",
		Package: "owner/platform", Version: "1.0.0", Digest: "test", Methods: []string{"GET /second"},
	}}})
	require.NoError(t, err)
	return state, second, string(raw)
}

func secondAt(address string) *basev0.NetworkMapping {
	return endpointMapping("platform", "second", "web", "http", "public", nativeInstance(address))
}

// The hop resolved second at the address it derived, and second's Init
// recorded another one before the freeze ran: the freeze refuses, naming both.
func TestHopFreezeRefusesARecordBetweenDerivationAndFreeze(t *testing.T) {
	ctx := context.Background()
	state, second, raw := hopFenceState(t)
	require.NoError(t, state.RecordNetworkMappings(ctx, second, []*basev0.NetworkMapping{secondAt("http://moved.native")}))
	err := state.freezeHopAddresses(raw, []*basev0.NetworkMapping{secondAt("http://derived.native")})
	require.ErrorContains(t, err, "hop route platform/second/web::http was resolved at [native http://derived.native], but platform/second initialized at [native http://moved.native]")
	require.Empty(t, state.routingMappings, "a refused freeze holds nothing")
}

// A frozen route is carried only when every mapping for its endpoint is the
// frozen one: a second, different mapping for the same endpoint is refused,
// whichever comes first.
func TestHopFrozenRouteRefusesADuplicateEndpointAtAnotherAddress(t *testing.T) {
	ctx := context.Background()
	derived, other := secondAt("http://derived.native"), secondAt("http://other.native")
	key := resources.EndpointDestination(derived.GetEndpoint())
	require.True(t, carriesMapping([]*basev0.NetworkMapping{derived}, key, resources.NetworkMappingHash(derived)))
	for _, offered := range [][]*basev0.NetworkMapping{{derived, other}, {other, derived}} {
		require.False(t, carriesMapping(offered, key, resources.NetworkMappingHash(derived)))
		state, second, raw := hopFenceState(t)
		require.NoError(t, state.freezeHopAddresses(raw, []*basev0.NetworkMapping{derived}))
		err := state.RecordNetworkMappings(ctx, second, offered)
		require.ErrorContains(t, err, "its Init accepted [native http://")
		_, recorded := state.GetNetworkMappingsFromUnique("platform/second")
		require.False(t, recorded)
	}
}

// A later delivery that resolves a routed service, still not initialized, at
// another address than the snapshot froze is refused as a move.
func TestHopFreezeRefusesARouteThatMovedBeforeItsOwnerInitialized(t *testing.T) {
	state, _, raw := hopFenceState(t)
	require.NoError(t, state.freezeHopAddresses(raw, []*basev0.NetworkMapping{secondAt("http://derived.native")}))
	require.NoError(t, state.freezeHopAddresses(raw, []*basev0.NetworkMapping{secondAt("http://derived.native")}), "the same address re-freezes")
	err := state.freezeHopAddresses(raw, []*basev0.NetworkMapping{secondAt("http://other.native")})
	require.ErrorContains(t, err, "stop and restart")
	require.ErrorContains(t, err, "hop route platform/second/web::http moved from [native http://derived.native], the address this run's hop snapshot holds, to [native http://other.native]")
}

func TestGeneratedHopRoutesRefusePrivatePublicationAndStaleArtifacts(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	t.Run("private", func(t *testing.T) {
		workspace := hopRouteWorkspace(t, "private")
		ctx := context.Background()
		env, err := SelectEnvironment(workspace, LocalEnvironmentName)
		require.NoError(t, err)
		module, err := workspace.LoadModuleFromName(ctx, "payments")
		require.NoError(t, err)
		service, err := module.LoadServiceFromName(ctx, "worker")
		require.NoError(t, err)
		flow, err := NewFlow(ctx, workspace, module, service, env, RunMode)
		if err == nil {
			defer flow.Stop()
			err = flow.checkConfigurationReferences(ctx, nil)
		}
		require.Error(t, err)
		require.Contains(t, err.Error(), "private")
	})
	for _, tc := range []struct{ name, path, from, to string }{
		{"digest", "contracts/api/first.json", "\"openapi\":\"3.0.0\"", "\"openapi\":\"3.0.1\""},
		{"version", composition.APIContractCatalogFileName, "\"version\":\"1.0.0\"", "\"version\":\"2.0.0\""},
		{"principal", "module.contract.codefly.yaml", "principal: platform", "principal: other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := hopRouteWorkspace(t, "public")
			module, err := workspace.LoadModuleFromName(context.Background(), "platform")
			require.NoError(t, err)
			path := filepath.Join(module.Dir(), tc.path)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), tc.from, tc.to, 1)), 0600))
			env, err := SelectEnvironment(workspace, LocalEnvironmentName)
			require.NoError(t, err)
			_, _, err = WorkspaceConfigurationsForChecking(context.Background(), workspace, env)
			require.Error(t, err)
		})
	}
}

func TestRoutingSnapshotCannotChangeDuringReloadOrIncrementalRefresh(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	workspace := hopRouteWorkspace(t, "public")
	flow := declaredReferenceFlow(t, workspace)
	require.NoError(t, flow.ConfigurationManager.Load(ctx, flow.world.Env.Runtime()))
	require.NoError(t, flow.CheckRoutingSnapshot(ctx, workspace))
	module, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	// Republish first's contract with another route: the document and the
	// digests the manifest and catalog name.
	files := []string{"contracts/api/first.json", "module.package.codefly.yaml", composition.APIContractCatalogFileName}
	saved := map[string][]byte{}
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Join(module.Dir(), file))
		require.NoError(t, err)
		saved[file] = raw
	}
	changed := strings.Replace(string(saved["contracts/api/first.json"]), `"/first"`, `"/changed"`, 1)
	before, after := composition.APIContractDigest(saved["contracts/api/first.json"]), composition.APIContractDigest([]byte(changed))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), files[0]), []byte(changed), 0600))
	for _, file := range files[1:] {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), file), []byte(strings.ReplaceAll(string(saved[file]), before, after)), 0600))
	}
	require.ErrorContains(t, flow.CheckRoutingSnapshot(ctx, workspace), "stop and restart")
	require.ErrorContains(t, flow.ConfigurationManager.Load(ctx, flow.world.Env.Runtime()), "stop and restart")
	// Restoring the selected inventory reuses the accepted snapshot.
	for file, raw := range saved {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), file), raw, 0600))
	}
	require.NoError(t, flow.CheckRoutingSnapshot(ctx, workspace))
	// The generated group cannot be replaced with an authored route table.
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations/local/hop-routes.env"), []byte("ROUTES={}\n"), 0600))
	require.ErrorContains(t, flow.CheckRoutingSnapshot(ctx, workspace), "remove its authored configuration")
}
