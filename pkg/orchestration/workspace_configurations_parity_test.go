package orchestration

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/codefly-dev/cli/pkg/remotenetwork"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

// parityWorld builds a World over the group-parity fixture in one mode, the way
// NewFlow builds one: a real configuration manager (the fixture's own
// configurations/local/* are the composition root's groups) with a loader adding
// the group a composed module would ship, real dependencies, shared state and
// both network managers. The run set is declared as InitManagers declares it, so
// a group's ${endpoint:…} reference resolves on either side.
func parityWorld(t *testing.T, mode Mode) (*World, *resources.Workspace) {
	t.Helper()
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/group-parity")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	manager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	// The fixture's own configurations/local/* through the loader every flow
	// registers (NewFlow): those groups are the composition root's.
	localReader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	require.NoError(t, err)
	manager.WithLoader(localReader)
	// `payments-store` is not named as composition-root: it is the group a
	// composed module ships for the one service that declares it.
	manager.WithLoader(staticWorkspaceLoader{confs: []*basev0.Configuration{
		workspaceConfiguration("payments-store", "gateway-endpoint", "${endpoint:platform/gateway/rest}"),
	}})
	require.NoError(t, manager.Load(ctx, env.Runtime()))

	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	sharedState, err := NewStateManager(ctx, manager, dependencies)
	require.NoError(t, err)
	localNetwork, err := network.NewRuntimeManager(ctx, manager)
	require.NoError(t, err)
	remoteNetwork, err := remotenetwork.NewRemoteManager(ctx, manager)
	require.NoError(t, err)

	world := &World{
		Mode: mode, Env: env, Workspace: workspace,
		ConfigurationManager: manager, SharedState: sharedState, Dependencies: dependencies,
		LocalNetworkManager: localNetwork, RemoteNetworkManager: remoteNetwork,
		runtimeContextFor: func(*resources.Service) string { return resources.RuntimeContextNative },
	}
	world.setRunProducers([]string{"platform/gateway", "payments/api", "payments/worker"}, nil)

	// A local run derives a producer's address from the endpoints it recorded at
	// Load; a render derives it from the producer's identity and namespace. Only
	// the run needs the recording, so only the run world gets it.
	if !world.deploys() {
		gateway := parityService(t, workspace, "platform", "gateway")
		identity, err := gateway.Identity()
		require.NoError(t, err)
		endpoints, err := gateway.LoadEndpoints(ctx)
		require.NoError(t, err)
		require.NoError(t, sharedState.RecordEndpoints(ctx, identity, endpoints))
	}
	return world, workspace
}

func parityService(t *testing.T, workspace *resources.Workspace, module, name string) *resources.Service {
	t.Helper()
	service, err := loadService(context.Background(), t, workspace, module, name)
	require.NoError(t, err)
	return service
}

// parityInstance is the agent instance each path carries its service in.
func parityInstance(t *testing.T, workspace *resources.Workspace, service *resources.Service) *coreservices.Instance {
	t.Helper()
	identity, err := service.Identity()
	require.NoError(t, err)
	identity.Workspace = workspace.Name
	return &coreservices.Instance{Workspace: workspace, Service: service, Identity: identity}
}

// groupSet is the set of workspace configuration group names a resolution
// delivered, sorted and deduplicated — what a service receives, independent of
// the addresses the values carry.
func groupSet(confs []*basev0.Configuration) []string {
	names := make([]string, 0, len(confs))
	for _, conf := range confs {
		for _, info := range conf.GetInfos() {
			if !slices.Contains(names, info.GetName()) {
				names = append(names, info.GetName())
			}
		}
	}
	slices.Sort(names)
	return names
}

// The one rule, enforced: the group set a service receives in the rendered
// deployment and the group set the same service receives under `codefly run` are
// the same set, for every service of a composition. The render is the source of
// truth and the run matches it.
//
// The asymmetry this guards against (core#688, cli#882): the deployed render
// resolved only the groups a service declares, while the run unioned those with
// the composition root's. A service reading a root-provided value then got it
// locally and never got it deployed — `payments/worker` below, which declares no
// group at all, received the whole root set under `run` and nothing at all in
// the render.
func TestRenderAndRunDeliverTheSameWorkspaceConfigurationGroups(t *testing.T) {
	ctx := context.Background()
	runWorld, workspace := parityWorld(t, RunMode)
	renderWorld, _ := parityWorld(t, SnapshotMode)

	runtimeContext, err := resources.NewRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, err)

	// What each service must receive, by the one rule: declared ∪ the
	// composition root's, each group once.
	for _, want := range []struct {
		module, name string
		groups       []string
	}{
		{"platform", "gateway", []string{"work-context"}},
		{"payments", "api", []string{"payments-store", "work-context"}},
		{"payments", "worker", []string{"work-context"}},
	} {
		expected := want.groups
		t.Run(want.module+"/"+want.name, func(t *testing.T) {
			service := parityService(t, workspace, want.module, want.name)
			instance := parityInstance(t, workspace, service)

			runMappings, err := runWorld.SharedState.GetDependenciesNetworkMappings(ctx, service)
			require.NoError(t, err)
			runner := &Runner{instance: instance, world: runWorld, runtimeContext: resources.RuntimeContextNative}
			run, err := runner.workspaceConfigurations(ctx, runMappings, runtimeContext)
			require.NoError(t, err)

			renderMappings, err := renderWorld.SharedState.GetDependenciesNetworkMappings(ctx, service)
			require.NoError(t, err)
			builder := &Builder{instance: instance, world: renderWorld}
			render, err := builder.workspaceConfigurations(ctx, renderMappings)
			require.NoError(t, err)

			require.Equal(t, expected, groupSet(run), "the run resolved a group set the rule does not describe")
			require.Equal(t, groupSet(run), groupSet(render),
				"a deployed service receives a different group set than the same service receives under run")
		})
	}
}

// The group set is identical; the addresses inside it are not, and must not be.
// A group naming ${endpoint:…} resolves to the producer's loopback address for a
// local run and to its in-cluster address in the render — the one difference the
// two paths are allowed, and the reason the parity above is asserted over the
// group set rather than over the values.
func TestRenderAndRunDifferOnlyInTheAddressFamily(t *testing.T) {
	ctx := context.Background()
	runWorld, workspace := parityWorld(t, RunMode)
	renderWorld, _ := parityWorld(t, SnapshotMode)
	runtimeContext, err := resources.NewRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, err)

	service := parityService(t, workspace, "payments", "api")
	instance := parityInstance(t, workspace, service)

	runner := &Runner{instance: instance, world: runWorld, runtimeContext: resources.RuntimeContextNative}
	run, err := runner.workspaceConfigurations(ctx, nil, runtimeContext)
	require.NoError(t, err)
	local, err := resources.GetConfigurationValue(ctx, run[0], "payments-store", "gateway-endpoint")
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`localhost:\d+$`), local)

	builder := &Builder{instance: instance, world: renderWorld}
	render, err := builder.workspaceConfigurations(ctx, nil)
	require.NoError(t, err)
	deployed, err := resources.GetConfigurationValue(ctx, render[0], "payments-store", "gateway-endpoint")
	require.NoError(t, err)
	require.NotEqual(t, local, deployed, "the render resolved the local address")
	require.NotContains(t, deployed, "localhost", "a deployed service cannot reach a producer on loopback")
}

// rootGroupWorld resolves one composition-root group, named `work-context`, into
// a bare service's deployed set — the render's own entry point, so what comes
// back is what the restricted render is handed.
func rootGroupWorld(t *testing.T, values ...*basev0.ConfigurationValue) []*basev0.Configuration {
	t.Helper()
	manager := loadedWorkspaceManager(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{{
			Origin: resources.ConfigurationWorkspace,
			Infos:  []*basev0.ConfigurationInformation{{Name: "work-context", ConfigurationValues: values}},
		}},
		rootConfigs: []string{"work-context"},
	})
	builder := &Builder{
		instance: &coreservices.Instance{Service: &resources.Service{}},
		world:    &World{Mode: SnapshotMode, ConfigurationManager: manager},
	}
	confs, err := builder.workspaceConfigurations(context.Background(), nil)
	require.NoError(t, err)
	return confs
}

// A composition-root group now reaches the render, so it is held to the render's
// own rules — which is the point of there being one resolved set. The
// consequence an operator meets: a credential-named value in a root group is
// promoted to a `secretKeyRef` on the service's own Secret, exactly as a
// declared group's has always been, and never rendered inline into a committed
// manifest. The value must therefore exist in the environment's secret store for
// the projected ExternalSecret to materialize — `codefly deploy secrets` seeds
// it from the render, so the key follows, but a store that was seeded before this
// change does not hold it yet.
func TestACompositionRootGroupRendersItsCredentialsByReference(t *testing.T) {
	for _, test := range []struct {
		name  string
		token *basev0.ConfigurationValue
	}{
		{"credential-named", &basev0.ConfigurationValue{Key: "authority-token", Value: "plaintext"}},
		{"declared secret", &basev0.ConfigurationValue{Key: "authority-token", Value: "plaintext", Secret: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			render := rootGroupWorld(t,
				&basev0.ConfigurationValue{Key: "authority-url", Value: "https://authority.example"},
				test.token,
			)
			require.Equal(t, []string{"work-context"}, groupSet(render),
				"the root group must reach the render at all")

			_, safe, references, err := promotableDeploymentConfigurations(&basev0.Configuration{}, render, "secret-api")
			require.NoError(t, err)
			require.NotEmpty(t, references, "the root group's credential must render as a reference")

			values := map[string]string{}
			for _, conf := range safe {
				for _, info := range conf.GetInfos() {
					for _, value := range info.GetConfigurationValues() {
						values[value.GetKey()] = value.GetValue()
					}
				}
			}
			require.Equal(t, "https://authority.example", values["authority-url"],
				"a non-credential value stays inline")
			require.Empty(t, values["authority-token"],
				"a credential must not survive into the rendered tree")
		})
	}
}
