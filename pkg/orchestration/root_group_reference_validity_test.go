package orchestration

import (
	"context"
	"os"
	"path/filepath"
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
	// The run set is the one a real flow computes for this origin: payments/worker
	// and its dependency closure, which is payments/worker alone — a root group's
	// reference adds no edge to it (Flow.managerDependencies). The producer is
	// deliberately NOT in it.
	//
	// An earlier revision hand-set {platform/authority, payments/worker} here,
	// and that is precisely what hid the blocker: the render's refusal was gated
	// on run membership, which a real render never grants a root-only producer,
	// so the test proved the refusal fired in a topology no render has.
	world.setRunProducers([]string{"payments/worker"}, nil)
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
// `private` and `internal`-without-allow-modules are the two refused forms.
// `visibility: module`, which the first review named, is a core value — a
// deprecated alias for internal with every module allow-listed (resources
// endpoint.go) — so it permits rather than refuses and is not a case here.
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
// It is refused HERE because this is the one place an operator can act on it
// instead of mid-run — before anything is built, pushed or started. It is NOT
// the only place: the resolution refuses it too
// (TestATypoedProducerInARootGroupIsRefusedByTheResolutionToo), because a guard
// that is only correct while a second guard is also correct is not a guard. An
// earlier revision had the read drop the value with a warning and left the
// refusal to this gate alone, which a dynamic review showed to be a fail-open.
func TestATypoedProducerInARootGroupIsRefusedAtThePlanGate(t *testing.T) {
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
	// The typo is NOT echoed. Core v0.11.0 carries no text from a value into a
	// diagnostic, because a reference is text from a value and a value may be a
	// secret — a mistyped producer is value text like any other. What locates it
	// is the group, the key and which reference of that value it was.
	require.NotContains(t, err.Error(), "platfrom", "a diagnostic must not echo the value")
	require.Contains(t, err.Error(), "work-context/authority-endpoint")
	require.Contains(t, err.Error(), "reference 1")
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

// The resolution refuses it too, rather than dropping it and relying on the gate
// having run.
//
// It used to drop: the read warned, the gate refused, and that division is the
// one this package has for declared groups. A dynamic review showed it to be a
// fail-open instead of a division — a typo supplied through an override reached the
// resolution while the gate was still reading the pre-override configurations,
// so nothing refused it anywhere and the value was quietly absent. A guard that
// is only correct while a second guard is also correct is not a guard, so core's
// verdict is propagated here as well.
func TestATypoedProducerInARootGroupIsRefusedByTheResolutionToo(t *testing.T) {
	world, service := referenceValidityWorld(t,
		referenceValidityWorkspace(t, "platfrom/authority/admin", "public"))

	_, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "a producer that is not a service of the workspace must be refused wherever the value resolves")
	require.NotContains(t, err.Error(), "platfrom", "a diagnostic must not echo the value")
	require.Contains(t, err.Error(), "not a service of this workspace")
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
	require.Contains(t, err.Error(), "a service of this workspace")
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
//
// Being a guard against this PR's own intermediate state is the whole of its
// claim: the tolerance also holds on the behaviour before the PR, so the test
// survives a wholesale rollback and proves nothing about the delta. Making
// root-only local derivation fatal again fails both subtests.
func TestARunWithTemporaryPortsDropsARootReferenceItCannotPlaceYet(t *testing.T) {
	for _, consumer := range []struct{ module, name string }{
		{"payments", "worker"},
		// The producer resolving its OWN root-group reference. It cannot be
		// repaired by ordering something before it: Runner.Init resolves the
		// workspace configurations before it generates its own proposed
		// mappings, so under --temporary-ports a service genuinely cannot know
		// its own ephemeral address at the moment it reads the group.
		{"platform", "authority"},
	} {
		t.Run(consumer.module+"/"+consumer.name, func(t *testing.T) {
			requireTemporaryPortsDropsTheRootReference(t, consumer.module, consumer.name)
		})
	}
}

func requireTemporaryPortsDropsTheRootReference(t *testing.T, module, name string) {
	t.Helper()
	world, _ := referenceValidityWorld(t,
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
	service, err := loadService(context.Background(), t, world.Workspace, module, name)
	require.NoError(t, err)

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

// A mixed group — one reference to a private endpoint, one to a producer that
// does not exist — reports both faults in one error.
//
// They arrive from core's check as one UnresolvedReferencesError and are
// propagated whole. An earlier shape filtered the nonexistent-producer entries
// out and returned nil when nothing was left, which would have delivered the
// private address whenever a typo sat beside it in the same group.
func TestAMixedRootGroupReportsBothTheVisibilityViolationAndTheTypo(t *testing.T) {
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: admin\n      api: rest\n      visibility: private\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": "typo=${endpoint:platfrom/authority/admin}\n" +
			"private=${endpoint:platform/authority/admin}\n",
	})
	world, service := referenceValidityWorld(t, workspace)

	_, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "a group with two faults must report both, not stop at one")
	require.Contains(t, err.Error(), "is private to module",
		"the visibility violation must be named")
	require.Contains(t, err.Error(), "not a service of this workspace",
		"and so must the typo's verdict: core's verdict on each reference is propagated whole")
	require.NotContains(t, err.Error(), "platfrom",
		"reported by position and reason, never by echoing the value")
}

// An unreadable workspace fails the reference check CLOSED.
//
// The lookup is memoized for the whole world, so a Debug line and a nil lookup
// would silence every reference check of every service of the run from one
// unreadable workspace — and a reference delivered unchecked is how a private
// endpoint's address reaches a service that may not see it. It is the same class
// as the unbound-bindings hazard, at the binding next to it.
func TestAnUnreadableWorkspaceFailsTheReferenceCheckClosed(t *testing.T) {
	workspace := referenceValidityWorkspace(t, "platform/authority/admin", "public")
	world, service := referenceValidityWorld(t, workspace)
	// Make the workspace unreadable the way a broken checkout is: the module
	// manifest its workspace file names is gone.
	require.NoError(t, os.Remove(filepath.Join(workspace.Dir(), "modules", "platform", "module.codefly.yaml")))

	_, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "an unreadable workspace must not mean references go unchecked")
	require.Contains(t, err.Error(), "cannot be checked against the producers")
}

// A reference supplied by an invocation-scoped override is checked like any
// other.
//
// The override is carried in CODEFLY__WORKSPACE_CONFIGURATION_OVERRIDES, core's
// SDK-to-CLI carrier, which an integration harness sets. It is not `--set`: that
// is a per-service runtime environment override, and a ${endpoint:…} written in
// one of those is neither checked nor interpolated.
//
// It escaped entirely before. The plan gate read the configurations off disk,
// which cannot see an override, so a typo'd producer an operator supplied on
// their own command line passed the plan; the resolution then dropped the value
// with no error, because a nonexistent producer was tolerated there and left to
// that gate. Two guards, each correct only while the other was, and the
// combination silent.
//
// Both halves are fixed and this asserts both: the gate reads an invocation-aware
// configuration (WorkspaceConfigurationsForChecking loads a reader, which is
// where core applies the overrides), and the resolution propagates core's
// verdict rather than deferring.
func TestAnInvocationOverrideIsCheckedLikeAnyOtherReference(t *testing.T) {
	ctx := context.Background()
	// The group on disk is correct; the override is what breaks it.
	workspace := referenceValidityWorkspace(t, "platform/authority/admin", "public")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	encoded, err := resources.EncodeWorkspaceConfigurationOverrides([]resources.WorkspaceConfigurationOverride{
		{Name: "work-context", Key: "authority-endpoint", Value: "${endpoint:platfrom/authority/admin}"},
	})
	require.NoError(t, err)
	t.Setenv(resources.WorkspaceConfigurationOverridesEnvironment, encoded)

	// The plan gate must see the overridden value, not the correct one on disk.
	checked, rootGroups, err := WorkspaceConfigurationsForChecking(ctx, workspace, env)
	require.NoError(t, err, "a loaded reader is what makes the override visible")
	require.Contains(t, rootGroups, "work-context")
	var sawOverride bool
	for _, info := range checked.Infos {
		for _, value := range info.GetConfigurationValues() {
			if value.GetValue() == "${endpoint:platfrom/authority/admin}" {
				sawOverride = true
			}
		}
	}
	require.True(t, sawOverride, "the gate is reading the pre-override configuration again")

	consumer, err := loadService(ctx, t, workspace, "payments", "worker")
	require.NoError(t, err)
	err = PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{consumer}, true)
	require.Error(t, err, "a typo supplied by an invocation override must not pass the plan")
	require.Contains(t, err.Error(), "not a service of this workspace")
	require.NotContains(t, err.Error(), "platfrom", "a diagnostic must not echo the value")

	// And the resolution refuses it too, so the gate is not the only guard.
	world, service := referenceValidityWorld(t, workspace)
	_, err = world.workspaceConfigurationsFor(ctx, service, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "the resolution must not depend on the gate having run")
	require.Contains(t, err.Error(), "not a service of this workspace")
}

// A RENDER refuses a root reference whose producer's addresses cannot be
// derived, with the DERIVATION's own reason — it does not fall back to the
// outcome check.
//
// The distinction matters because the outcome check would refuse this too, for a
// different reason ("the value was dropped"), which is why making the derivation
// non-fatal in a render survived every test: safety held, the claim did not. The
// two differ in what they tell an operator. The derivation names the producer
// whose address could not be computed and why; the outcome names a value that is
// missing and leaves the cause to be guessed. A render must give the first, and
// a reviewer must be able to tell which one fired.
//
// The failure is produced inside the real manager rather than by removing it:
// a remote network manager with no DNS source cannot generate a mapping and says
// so. TestARenderRefusesARootReferenceNoAddressCanBeDerivedFor above covers the
// other shape — no manager at all, which returns no mappings and no error, so
// the outcome check is what refuses there. Both must refuse; only this one
// carries the derivation's reason.
func TestARenderRefusesARootReferenceWithTheDerivationsOwnReason(t *testing.T) {
	ctx := context.Background()
	world, service := referenceValidityWorld(t,
		referenceValidityWorkspace(t, "platform/authority/admin", "public"),
		func(world *World) {
			manager, err := remotenetwork.NewRemoteManager(ctx, nil)
			require.NoError(t, err)
			world.RemoteNetworkManager = manager
		})
	require.True(t, world.deploys(), "the mode is what makes a derivation failure fatal")

	_, err := world.workspaceConfigurationsFor(ctx, service, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "a render must not emit a manifest with a root group's value silently missing")
	require.Contains(t, err.Error(), "cannot derive the addresses of platform/authority",
		"a render must refuse with the derivation's reason, not with the outcome check's")
	require.NotContains(t, err.Error(), "did not survive resolution",
		"the outcome check's refusal must not be what an operator reads here")
}

// A World with no workspace refuses to resolve a group carrying a reference,
// rather than resolving it unchecked.
//
// This is the last fail-open in this file, and it had the same shape as all the
// others: the thing that answers the question was absent, so the question went
// unasked. `workspaceProducers` returns no lookup without a workspace, and the
// reference check then returned nil — so a mapping for a PRIVATE endpoint of
// another module was interpolated into the value and delivered with err=nil,
// which is the bypass TestARootGroupReferenceIsHeldToTheProducersExportBoundary
// exists to prevent for every other World.
//
// NewFlow always binds a workspace, so no production path reaches this. That is
// the argument for refusing rather than for trusting it to stay unreachable:
// "unreachable" is a property of today's callers, and the cost of being wrong
// about it is a private address delivered across a module boundary in silence.
// (Layer-4 round four, E3.)
func TestAWorldWithNoWorkspaceRefusesToResolveAReferenceBearingGroup(t *testing.T) {
	world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("platform", "gateway-endpoint", "http://${endpoint:saas/auth-gateway/rest}"),
		},
	})
	require.Nil(t, world.Workspace, "the case is a World with nothing to check references against")
	// A consumer outside the producer's module, and a mapping for an endpoint
	// the producer declares private: everything needed to deliver an address
	// this consumer may not have.
	consumer := &resources.Service{WorkspaceConfigurationDependencies: []string{"platform"}}
	mappings := []*basev0.NetworkMapping{{
		Endpoint: &basev0.Endpoint{
			Module: "saas", Service: "auth-gateway", Name: "rest", Api: "rest", Visibility: "private",
		},
		Instances: []*basev0.NetworkInstance{
			{Address: "localhost:38342", Access: resources.NewNativeNetworkAccess()},
		},
	}}

	confs, err := world.workspaceConfigurationsFor(context.Background(), consumer, mappings, resources.NewNativeNetworkAccess())
	require.Error(t, err, "a reference must not be resolved by a World that cannot check it")
	require.Contains(t, err.Error(), "has no workspace")
	require.Contains(t, err.Error(), "saas/auth-gateway/rest", "the refusal must name the reference it could not check")
	value, delivered := groupValue(confs, "platform", "gateway-endpoint")
	require.False(t, delivered, "nothing is delivered when nothing could be checked")
	require.NotContains(t, value, "38342", "least of all the private endpoint's address")
}
