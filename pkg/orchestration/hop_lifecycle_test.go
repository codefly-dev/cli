package orchestration

import (
	"context"
	"encoding/json"
	"github.com/codefly-dev/authority/libraries/hop/go/routing"
	"github.com/codefly-dev/core/composition"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunClosureIncludesGeneratedRouteProducers(t *testing.T) {
	ctx := context.Background()
	workspace := hopRouteWorkspace(t, "public")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	mod, err := workspace.LoadModuleFromName(ctx, "payments")
	require.NoError(t, err)
	svc, err := mod.LoadServiceFromName(ctx, "worker")
	require.NoError(t, err)
	flow, err := NewFlow(ctx, workspace, mod, svc, env, RunMode, WithRunModuleClosure("payments"))
	require.NoError(t, err)
	defer flow.Stop()
	require.NoError(t, flow.checkConfigurationReferences(ctx, nil))
	deps, err := flow.managerDependencies(ctx)
	require.NoError(t, err)
	var uniques []string
	for _, dep := range deps {
		uniques = append(uniques, dep.Unique)
	}
	require.Contains(t, uniques, "platform/first")
	require.Contains(t, uniques, "platform/second")
}

func TestWatchReloadRejectsChangedInventoryBeforeStopping(t *testing.T) {
	ctx := context.Background()
	ws := hopRouteWorkspace(t, "public")
	flow := declaredReferenceFlow(t, ws)
	require.NoError(t, flow.ConfigurationManager.Load(ctx, flow.world.Env.Runtime()))
	mod, err := ws.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	path := filepath.Join(mod.Dir(), composition.APIContractCatalogFileName)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	artifactPath := filepath.Join(mod.Dir(), "contracts/api/first.json")
	artifact, err := os.ReadFile(artifactPath)
	require.NoError(t, err)
	changed := []byte(strings.Replace(string(artifact), `"/first"`, `"/changed"`, 1))
	oldDigest, newDigest := composition.APIContractDigest(artifact), composition.APIContractDigest(changed)
	require.NoError(t, os.WriteFile(artifactPath, changed, 0600))
	updated := strings.Replace(string(raw), `"path":"/first"`, `"path":"/changed"`, 1)
	updated = strings.ReplaceAll(updated, oldDigest, newDigest)
	require.NoError(t, os.WriteFile(path, []byte(updated), 0600))
	manifestPath := filepath.Join(mod.Dir(), "module.package.codefly.yaml")
	manifest, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, []byte(strings.ReplaceAll(string(manifest), oldDigest, newDigest)), 0600))
	_, _, err = WorkspaceConfigurationsForChecking(ctx, ws, flow.world.Env)
	require.NoError(t, err)
	require.ErrorContains(t, flow.CheckRoutingSnapshot(ctx, ws), "stop and restart")
	client := &restartRuntimeClient{}
	runner := runnerWithRuntimeClient(client)
	runner.world = flow.world
	runner.instance.Identity.Module = "platform"
	runner.instance.Identity.Name = "first"
	runner.markStarted()
	callbacks := 0
	runner.callback = func(context.Context, Action) error { callbacks++; return nil }
	err = runner.handleDesiredState(ctx, runtimev0.DesiredState_START)

	require.ErrorContains(t, err, "stop and restart")
	require.Zero(t, client.stopCalls)
	require.Zero(t, callbacks)
	// A refused reload can be retried after restoring the accepted publication.
	require.NoError(t, os.WriteFile(artifactPath, artifact, 0600))
	require.NoError(t, os.WriteFile(path, raw, 0600))
	require.NoError(t, os.WriteFile(manifestPath, manifest, 0600))
	require.NoError(t, runner.handleDesiredState(ctx, runtimev0.DesiredState_START))
	require.Equal(t, 1, client.stopCalls)
	require.Equal(t, 1, callbacks)
}

// A Runtime.Init response is only a proposal until the shared address fence
// accepts it. Rejection preserves both published mappings and reserved ports.
func TestInitCannotReplaceAnAddressAlreadyDeliveredToHop(t *testing.T) {
	ctx := context.Background()
	runner, world := gatewayRunner(t, startRealAgent(t, realAgentBind))
	previous, err := world.LocalNetworkManager.GenerateNetworkMappings(ctx, world.Env.Runtime(), world.Workspace, runner.instance.Identity, runner.endpoints, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	require.NoError(t, world.SharedState.RecordNetworkMappings(ctx, runner.instance.Service, previous))
	routes := routing.Routing{Schema: routing.RoutingSchema, Endpoints: []routing.RouteEndpoint{{Audience: "web", Service: "gateway", Endpoint: "rest", Transport: "http", Address: nativeAddressFor(t, previous, "rest"), Package: "test/gateway", Version: "1", Digest: "test", Methods: []string{"GET /"}}}}
	raw, err := json.Marshal(routes)
	require.NoError(t, err)
	require.NoError(t, world.SharedState.freezeHopAddresses(string(raw), previous))
	runner.networkMappings = previous
	runner.pinAcceptedPorts(previous)
	_, err = runner.Init(ctx)
	require.ErrorContains(t, err, "stop and restart")
	require.Equal(t, previous, runner.networkMappings)
	recorded, err := world.SharedState.GetNetworkMappings(ctx, runner.instance.Identity)
	require.NoError(t, err)
	require.Equal(t, previous, recorded)
	exposed, err := world.ConfigurationManager.GetSharedServiceConfiguration(ctx, runner.instance.Identity.Unique())
	require.NoError(t, err)
	require.Empty(t, exposed)
	retry, err := world.LocalNetworkManager.GenerateNetworkMappings(ctx, world.Env.Runtime(), world.Workspace, runner.instance.Identity, runner.endpoints, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	for _, name := range []string{"rest", "grpc"} {
		require.Equal(t, hostOfNativeInstance(t, previous, name), hostOfNativeInstance(t, retry, name))
	}
}

func TestRemoteInitCannotPublishAnAddressRejectedByHop(t *testing.T) {
	ctx := context.Background()
	runner, world := gatewayRunner(t, startRealAgent(t, realAgentEcho))
	previous, err := world.LocalNetworkManager.GenerateNetworkMappings(ctx, world.Env.Runtime(), world.Workspace, runner.instance.Identity, runner.endpoints, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	require.NoError(t, world.SharedState.RecordNetworkMappings(ctx, runner.instance.Service, previous))
	routes := routing.Routing{Schema: routing.RoutingSchema, Endpoints: []routing.RouteEndpoint{{Audience: "web", Service: "gateway", Endpoint: "rest", Transport: "http", Address: nativeAddressFor(t, previous, "rest"), Package: "test/gateway", Version: "1", Digest: "test", Methods: []string{"GET /"}}}}
	raw, err := json.Marshal(routes)
	require.NoError(t, err)
	require.NoError(t, world.SharedState.freezeHopAddresses(string(raw), previous))
	runner.networkMappings = previous
	// A changed remote mapping is rejected before replacing this runner's view.
	world.LocalNetworkManager.WithPortOverrides(map[string]uint16{"web/gateway/rest": freePort(t), "web/gateway/grpc": freePort(t)})
	_, err = runner.InitRemote(ctx)
	require.ErrorContains(t, err, "stop and restart")
	require.Equal(t, previous, runner.networkMappings)
	recorded, err := world.SharedState.GetNetworkMappings(ctx, runner.instance.Identity)
	require.NoError(t, err)
	require.Equal(t, previous, recorded)
}
