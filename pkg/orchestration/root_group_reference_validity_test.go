package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/remotenetwork"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// referenceValidityWorkspace is a two-module composition whose composition-root
// group carries one ${endpoint:…} the test chooses, read by a consumer in the
// OTHER module that declares no group at all. Two modules, because
// resources.ValidateEndpointVisibility returns nil within one module: a
// cross-module consumer is the only one an export boundary applies to.
func referenceValidityWorkspace(t *testing.T, reference string, producerVisibility string) *resources.Workspace {
	t.Helper()
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: admin\n      api: rest\n      visibility: " + producerVisibility + "\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		// Declares nothing: whatever it receives is the composition root's, which
		// is exactly the path that used to have no check on it.
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:" + reference + "}\n",
	})
}

// referenceValidityWorld resolves that composition for payments/worker, through
// the render's entry point, with the World bound the way NewFlow binds one.
func referenceValidityWorld(t *testing.T, workspace *resources.Workspace, options ...func(*World)) (*World, *resources.Service) {
	t.Helper()
	ctx := context.Background()
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	manager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	localReader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	require.NoError(t, err)
	manager.WithLoader(localReader)
	require.NoError(t, manager.Load(ctx, env.Runtime()))

	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	sharedState, err := NewStateManager(ctx, manager, dependencies)
	require.NoError(t, err)
	remoteNetwork, err := remotenetwork.NewRemoteManager(ctx, manager)
	require.NoError(t, err)
	localNetwork, err := network.NewRuntimeManager(ctx, manager)
	require.NoError(t, err)

	world := &World{
		Mode: SnapshotMode, Env: env, Workspace: workspace,
		ConfigurationManager: manager, SharedState: sharedState, Dependencies: dependencies,
		RemoteNetworkManager:  remoteNetwork,
		LocalNetworkManager:   localNetwork,
		runtimeContextFor:     func(*resources.Service) string { return resources.RuntimeContextNative },
		compositionRootGroups: localReader.CompositionRootWorkspaceConfigurationNames,
		providedWorkspaceConfigurationInfos: func() []*basev0.ConfigurationInformation {
			return workspaceConfigurationInfos(localReader.Configurations())
		},
	}
	world.setRunProducers([]string{"platform/authority", "payments/worker"}, nil)
	for _, option := range options {
		option(world)
	}

	service, err := loadService(ctx, t, workspace, "payments", "worker")
	require.NoError(t, err)
	return world, service
}

// recordParityEndpoints records a producer's declared endpoints on the shared
// state, which is what a service's Load does and what makes it a producer of
// THIS run as far as producerNetworkMappings is concerned.
func recordParityEndpoints(t *testing.T, world *World, module, name string) {
	t.Helper()
	ctx := context.Background()
	service, err := loadService(ctx, t, world.Workspace, module, name)
	require.NoError(t, err)
	identity, err := service.Identity()
	require.NoError(t, err)
	endpoints, err := service.LoadEndpoints(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, endpoints)
	require.NoError(t, world.SharedState.RecordEndpoints(ctx, identity, endpoints))
}

// A composition-root group is not a way around a producer's export boundary.
//
// This is the hole the shared resolution opened and this closes. Core's
// plan-time check (configurations.CheckEndpointReferences) validates endpoint
// visibility over consumer.WorkspaceConfigurationDependencies — the DECLARED
// groups only — and its own comment says why the rule exists at all: "without
// this, declaring the group instead of the dependency would be the way around
// visibility". Before the shared resolution, a service that did not declare a
// root group never had the producer's mappings bound, so the boundary held by
// accident. Once every root-referenced producer is bound for every service, a
// root group carrying a reference to a private endpoint would reach every
// service of the composition with nothing refusing it.
//
// `private` and `internal`-without-allow-modules are the two refused forms (the
// review called the case `visibility: module`; core's values are external,
// public, internal and private, and it is the latter two that refuse).
func TestARootGroupReferenceIsHeldToTheProducersExportBoundary(t *testing.T) {
	for _, visibility := range []string{"private", "internal"} {
		t.Run(visibility, func(t *testing.T) {
			world, service := referenceValidityWorld(t,
				referenceValidityWorkspace(t, "platform/authority/admin", visibility))

			_, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewContainerNetworkAccess())
			require.Error(t, err, "a root group must not carry an endpoint a declared dependency on it would be refused")
			require.Contains(t, err.Error(), "admin")
			require.Contains(t, err.Error(), "payments", "the refusal must name the module that may not see it")
		})
	}
}

// The same group with a public endpoint resolves, so the test above is about the
// boundary and not about root references failing in general.
func TestARootGroupReferenceToAPublicEndpointResolves(t *testing.T) {
	world, service := referenceValidityWorld(t,
		referenceValidityWorkspace(t, "platform/authority/admin", "public"))

	confs, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	address, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.True(t, delivered, "a visible root reference must still reach a service that declares no group")
	requireInClusterAddressIn(t, address, "boundary", "platform", "authority", LocalEnvironmentName)
}

// A typo'd producer in a composition-root group is refused by name, at the plan
// gate, before anything is built or started.
//
// It was the worst case of the silent drop: a root group is the one kind no
// service declares, so the plan gate — which read declared groups only — never
// saw it, core's run-wide interpolation drops what it cannot resolve at DEBUG,
// and the value went missing from EVERY service of the composition with nothing
// said anywhere. The gate now covers the effective set, so a root group's typo
// gets the same answer a declared group's always had.
//
// It is refused HERE rather than inside the resolution on purpose. A producer
// that is not a service of the workspace is a producer of no run, so the read
// drops it for the consumer (and warns) while the gate refuses the plan — the
// division this package already had for declared groups, and the one place an
// operator can act on it instead of mid-run.
func TestATypoedProducerInARootGroupIsRefusedByNameAtThePlanGate(t *testing.T) {
	ctx := context.Background()
	workspace := referenceValidityWorkspace(t, "platfrom/authority/admin", "public")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	consumer, err := loadService(ctx, t, workspace, "payments", "worker")
	require.NoError(t, err)
	require.Empty(t, consumer.WorkspaceConfigurationDependencies,
		"the case is a consumer that declares nothing, so only the effective set can reach the typo")

	err = PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{consumer}, true)
	require.Error(t, err, "a reference naming a producer the workspace does not have must not reach a run")
	require.Contains(t, err.Error(), "platfrom/authority")
	require.Contains(t, err.Error(), "not a service of this workspace")

	// And the same plan passes once the producer name is right, so the refusal is
	// about the typo and not about root groups reaching the gate at all.
	ok := referenceValidityWorkspace(t, "platform/authority/admin", "public")
	okEnv, err := SelectEnvironment(ok, LocalEnvironmentName)
	require.NoError(t, err)
	okConsumer, err := loadService(ctx, t, ok, "payments", "worker")
	require.NoError(t, err)
	require.NoError(t, PlanConfigurationReferences(ctx, ok, okEnv, []*resources.Service{okConsumer}, true))
}

// The resolution, meanwhile, drops that same value rather than failing a run
// mid-flight — and says so at WARN instead of core's DEBUG.
func TestATypoedProducerInARootGroupIsDroppedByTheResolution(t *testing.T) {
	world, service := referenceValidityWorld(t,
		referenceValidityWorkspace(t, "platfrom/authority/admin", "public"))

	confs, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err, "a producer of no run does not fail the read; the plan gate is what refuses it")
	_, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.False(t, delivered, "the value cannot resolve, so it is not delivered")
}

// An endpoint the producer does not declare is refused too, by core's own
// reason, which names what the producer does declare. This replaces the
// resolution's own mappingsCarryProducer heuristic ("its other endpoints
// resolved, so the reference is wrong"): core answers it from the manifest
// rather than from whichever mappings happened to be bound, so the verdict no
// longer depends on resolution order.
func TestAnEndpointTheProducerDoesNotDeclareIsRefusedByName(t *testing.T) {
	world, service := referenceValidityWorld(t,
		referenceValidityWorkspace(t, "platform/authority/metrics", "public"))

	_, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err)
	require.Contains(t, err.Error(), "declares no such endpoint")
	require.Contains(t, err.Error(), "admin", "the reason must name what the producer does declare")
}

// The render refuses a root reference whose producer is part of the deployment
// and whose address nothing could derive — over a World with real dependencies
// and real shared state, so producer discovery actually ran and came back
// empty.
//
// That matters because the refusal's whole claim is about what discovery could
// not do. A World without Dependencies short-circuits discovery before it
// starts, so the refusal would fire for a reason no real flow ever has:
// NewFlow always builds both. Here the only thing missing is the thing being
// tested — nothing can turn the producer's identity into an address.
func TestARenderRefusesARootReferenceNoAddressCanBeDerivedFor(t *testing.T) {
	world, service := referenceValidityWorld(t,
		referenceValidityWorkspace(t, "platform/authority/admin", "public"),
		// A render derives a deployed address from the producer's identity and
		// namespace through this manager. Without it nothing can, which is the
		// case under test: the manifest would simply lack the value.
		func(world *World) { world.RemoteNetworkManager = nil })
	require.NotNil(t, world.Dependencies, "discovery must be able to run for the refusal to be about discovery")
	require.NotNil(t, world.SharedState)

	_, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "a render must not emit a manifest with a root group's value silently missing")
	require.Contains(t, err.Error(), "${endpoint:platform/authority/admin}")
	require.Contains(t, err.Error(), "work-context/authority-endpoint", "the refusal must name the value that went missing")
	require.Contains(t, err.Error(), "which this deployment contains")
}

// The same unresolved reference under `codefly run` is not refused — it is
// dropped, and warned about. The producer is in the run and has recorded no
// endpoint, which is what --temporary-ports guarantees for anything that has
// not initialized yet, and a root group's reference orders nothing. Refusing
// here would fail a run whose consumer merely starts first.
//
// Also the regression guard for that: before this revision,
// referencedProducerMappings treated any failure to derive as fatal, so a
// composition whose root group held a reference could not be run with
// --temporary-ports at all (`codefly ci`, `test --temporary-ports`). The run
// below sets exactly that flag.
func TestARunWithTemporaryPortsDropsARootReferenceItCannotPlaceYet(t *testing.T) {
	world, service := referenceValidityWorld(t,
		referenceValidityWorkspace(t, "platform/authority/admin", "public"),
		func(world *World) {
			world.Mode = RunMode
			// The producer recorded its endpoints at Load, as every service of a
			// run does, so producerNetworkMappings really does reach
			// localProducerMappings — which is where --temporary-ports says it
			// cannot know an address allocated at initialization. Without the
			// recording the call returns early and the branch under test is
			// never entered.
			recordParityEndpoints(t, world, "platform", "authority")
			world.temporaryPorts = true
		})

	confs, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err, "a run must not fail because a root group's reference is not placeable yet")
	_, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.False(t, delivered, "the value is dropped, and warned about, not invented")
}

// requireKnownRootGroup, directly: a World whose root-group source cannot name a
// group the manager delivers run-wide must refuse rather than ship it.
//
// It is the one guard with no natural call site — it fires only when a World was
// built without binding compositionRootGroups, which NewFlow always does — so it
// is exercised here on its own. The failure it prevents is the quiet one: that
// group's ${endpoint:…} references were resolved against mappings nobody bound
// for them, so its values are silently absent from the workload.
func TestRequireKnownRootGroupRefusesAGroupTheResolutionDidNotPlanFor(t *testing.T) {
	world := &World{}
	conf := &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos:  []*basev0.ConfigurationInformation{{Name: "work-context"}},
	}

	require.NoError(t, world.requireKnownRootGroup(conf, []string{"work-context"}),
		"a group the resolution planned for is delivered")

	err := world.requireKnownRootGroup(conf, []string{"payments-store"})
	require.Error(t, err, "a root group the resolution never named must not be delivered")
	require.Contains(t, err.Error(), "work-context")
	require.Contains(t, err.Error(), "CompositionRootWorkspaceConfigurationNames",
		"the refusal must name the binding whose absence caused it")
}
