package orchestration

import (
	"context"
	"maps"
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
		// The same binding NewFlow makes, from the same loader: the fixture's own
		// configurations/local/* are the composition root's groups, and the
		// resolution plans producer discovery against their names.
		compositionRootGroups: localReader.CompositionRootWorkspaceConfigurationNames,
	}
	world.setRunProducers([]string{"platform/gateway", "platform/authority", "payments/api", "payments/worker"}, nil)

	// A local run derives a producer's address from the endpoints it recorded at
	// Load; a render derives it from the producer's identity and namespace. Only
	// the run needs the recording, so only the run world gets it. Both producers
	// record: platform/gateway is the one a declared group names, and
	// platform/authority the one only the composition root's group names.
	if !world.deploys() {
		for _, producer := range []string{"gateway", "authority"} {
			service := parityService(t, workspace, "platform", producer)
			identity, err := service.Identity()
			require.NoError(t, err)
			endpoints, err := service.LoadEndpoints(ctx)
			require.NoError(t, err)
			require.NoError(t, sharedState.RecordEndpoints(ctx, identity, endpoints))
		}
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
//
// It deduplicates, so it is the wrong instrument for the delivered-once claim:
// a group emitted twice reads here exactly like a group emitted once. Use
// groupCounts for that.
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

// groupCounts is how many times each group name was delivered. A name the
// service both declares and the composition root provides is resolved from two
// sets, so "delivered once" is a claim about this map and not about groupSet.
func groupCounts(confs []*basev0.Configuration) map[string]int {
	counts := map[string]int{}
	for _, conf := range confs {
		for _, info := range conf.GetInfos() {
			counts[info.GetName()]++
		}
	}
	return counts
}

// groupValue reads one key of one group out of a resolution, reporting whether
// the key was delivered at all. resources.GetConfigurationValue answers "" and
// no error for an absent key, which is precisely the omission these tests are
// about: it cannot tell a dropped value from an empty one.
func groupValue(confs []*basev0.Configuration, group, key string) (string, bool) {
	for _, conf := range confs {
		for _, info := range conf.GetInfos() {
			if info.GetName() != group {
				continue
			}
			for _, value := range info.GetConfigurationValues() {
				if value.GetKey() == key {
					return value.GetValue(), true
				}
			}
		}
	}
	return "", false
}

// The one rule, enforced: the group set a service receives in the rendered
// deployment and the group set the same service receives under `codefly run` are
// the same set, for every service of a composition — and so is every value
// inside it, up to the address family. The render is the source of truth and the
// run matches it.
//
// The asymmetry this guards against (core#688, cli#882): the deployed render
// resolved only the groups a service declares, while the run unioned those with
// the composition root's. A service reading a root-provided value then got it
// locally and never got it deployed — `payments/worker` below, which declares no
// group at all, received the whole root set under `run` and nothing at all in
// the render.
//
// Group names alone do not establish that. `work-context` carries a literal
// (`authority-url`), so its name survives a resolution that drops every
// ${endpoint:…} it holds: a name-set assertion passes while the value a service
// actually dials is gone. The keys are asserted per group for that reason, and
// `authority-endpoint` — a reference in a group only `platform/gateway` declares
// — is the key that exercises it.
func TestRenderAndRunDeliverTheSameWorkspaceConfigurationGroups(t *testing.T) {
	ctx := context.Background()
	runWorld, workspace := parityWorld(t, RunMode)
	renderWorld, _ := parityWorld(t, SnapshotMode)

	runtimeContext, err := resources.NewRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, err)

	// What each service must receive, by the one rule: declared ∪ the
	// composition root's, each group once, and every key of each group.
	for _, want := range []struct {
		module, name string
		groups       []string
		keys         map[string][]string
	}{
		{"platform", "gateway", []string{"work-context"},
			map[string][]string{"work-context": {"authority-url", "authority-endpoint", "authority-token"}}},
		{"payments", "api", []string{"payments-store", "work-context"},
			map[string][]string{
				"work-context":   {"authority-url", "authority-endpoint", "authority-token"},
				"payments-store": {"gateway-endpoint"},
			}},
		{"payments", "worker", []string{"work-context"},
			map[string][]string{"work-context": {"authority-url", "authority-endpoint", "authority-token"}}},
		// The producer the root group references is itself a consumer of it: it
		// declares no group, and the reference it receives names its own
		// endpoint. Covered so no service of the fixture is left unasserted.
		{"platform", "authority", []string{"work-context"},
			map[string][]string{"work-context": {"authority-url", "authority-endpoint", "authority-token"}}},
	} {
		expected, expectedKeys := want.groups, want.keys
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

			// Delivered once. `platform/gateway` declares `work-context`, which is
			// also the composition root's own, so it is resolved from both sets and
			// this is the assertion the union's deduplication answers to. groupSet
			// cannot make it: it deduplicates.
			for _, group := range expected {
				require.Equal(t, 1, groupCounts(run)[group], "the run delivered %s more than once", group)
				require.Equal(t, 1, groupCounts(render)[group], "the render delivered %s more than once", group)
			}

			// Every key of every group, in both. A group name that survives with
			// its reference values dropped is the omission this guards.
			for group, keys := range expectedKeys {
				for _, key := range keys {
					_, inRun := groupValue(run, group, key)
					require.True(t, inRun, "the run did not deliver %s/%s", group, key)
					_, inRender := groupValue(render, group, key)
					require.True(t, inRender, "the render did not deliver %s/%s", group, key)
				}
			}
		})
	}
}

// The gap this PR closes, stated as its own case: a composition-root group whose
// value is a ${endpoint:…} reference, read by a service that declares no group
// at all.
//
// Producer discovery used to be handed the declared groups only, and core
// resolves the root's run-wide groups leniently — a value whose reference this
// consumer cannot resolve is dropped for it rather than failing it (#393). So
// `payments/worker`, declaring nothing, had the gateway's mappings bound for
// nothing, and `authority-endpoint` was deleted from its set with no error
// anywhere: the empty-configuration fault, reached through the shared resolver
// instead of through the old declared-only render. Verified failing before the
// fix — the key was absent from both paths while `authority-url`, a literal,
// came through and kept the group's name alive.
func TestARootOnlyEndpointReferenceReachesAServiceThatDeclaresNoGroup(t *testing.T) {
	ctx := context.Background()
	runWorld, workspace := parityWorld(t, RunMode)
	renderWorld, _ := parityWorld(t, SnapshotMode)
	runtimeContext, err := resources.NewRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, err)

	service := parityService(t, workspace, "payments", "worker")
	require.Empty(t, service.WorkspaceConfigurationDependencies,
		"the case is a service that declares no group; the fixture must keep it that way")
	instance := parityInstance(t, workspace, service)

	runner := &Runner{instance: instance, world: runWorld, runtimeContext: resources.RuntimeContextNative}
	run, err := runner.workspaceConfigurations(ctx, nil, runtimeContext)
	require.NoError(t, err)
	local, delivered := groupValue(run, "work-context", "authority-endpoint")
	require.True(t, delivered, "the run dropped a root group's endpoint value for an undeclared consumer")
	require.Regexp(t, regexp.MustCompile(`localhost:\d+$`), local)

	builder := &Builder{instance: instance, world: renderWorld}
	render, err := builder.workspaceConfigurations(ctx, nil)
	require.NoError(t, err)
	deployed, delivered := groupValue(render, "work-context", "authority-endpoint")
	require.True(t, delivered, "the render dropped a root group's endpoint value for an undeclared consumer")
	requireInClusterAddress(t, deployed, "platform", "authority")
}

// requireInClusterAddress asserts a deployed address is the producer's
// in-cluster one: its Kubernetes service DNS name and port.
//
// "does not contain localhost" does not establish that. A loopback address
// written as 127.0.0.1 passes such a check while being exactly the address a
// deployed workload cannot reach, so the assertion names the form it wants
// instead of the one form it rejects.
func requireInClusterAddress(t *testing.T, address, module, service string) {
	t.Helper()
	require.Regexp(t, regexp.MustCompile(`^(?:[a-z0-9-]+://)?`+regexp.QuoteMeta(service)+`\.[a-z0-9-]*`+regexp.QuoteMeta(module)+`[a-z0-9-]*\.svc\.cluster\.local:\d+$`),
		address, "a deployed consumer must reach %s/%s at its in-cluster service DNS name", module, service)
}

// The group set is identical; the addresses inside it are not, and must not be.
// A group naming ${endpoint:…} resolves to the producer's loopback address for a
// local run and to its in-cluster address in the render — the one difference the
// two paths are allowed, and the reason the parity above is asserted over the
// group set and the delivered keys rather than over the values.
//
// Both kinds of group are exercised: `payments-store`, which the consumer
// declares, and `work-context`, which only the composition root provides. The
// declared one alone would pass against the pre-change resolution, which
// resolved declared groups in both paths already; the root one is what this
// change makes resolvable at all, and it is only that if it names a producer no
// declared group names. `payments-store` names platform/gateway, so a root
// reference to platform/gateway would be bound by that group's own discovery
// and resolve pre-change too — verified, which is why the root group references
// platform/authority instead.
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

	builder := &Builder{instance: instance, world: renderWorld}
	render, err := builder.workspaceConfigurations(ctx, nil)
	require.NoError(t, err)

	for _, reference := range []struct{ group, key, module, producer string }{
		{"payments-store", "gateway-endpoint", "platform", "gateway"},
		{"work-context", "authority-endpoint", "platform", "authority"},
	} {
		t.Run(reference.group+"/"+reference.key, func(t *testing.T) {
			local, delivered := groupValue(run, reference.group, reference.key)
			require.True(t, delivered, "the run did not deliver the reference at all")
			require.Regexp(t, regexp.MustCompile(`localhost:\d+$`), local)

			deployed, delivered := groupValue(render, reference.group, reference.key)
			require.True(t, delivered, "the render did not deliver the reference at all")
			require.NotEqual(t, local, deployed, "the render resolved the local address")
			requireInClusterAddress(t, deployed, reference.module, reference.producer)
		})
	}

	// A value that is not an address is the same string in both: the address
	// family is the *only* difference, which a test over addresses alone
	// cannot say.
	for _, literal := range []struct{ group, key string }{
		{"work-context", "authority-url"},
	} {
		runValue, delivered := groupValue(run, literal.group, literal.key)
		require.True(t, delivered)
		renderValue, delivered := groupValue(render, literal.group, literal.key)
		require.True(t, delivered)
		require.Equal(t, runValue, renderValue, "%s/%s is not an address and must not differ", literal.group, literal.key)
	}
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
	world := &World{Mode: SnapshotMode, ConfigurationManager: manager}
	world.compositionRootGroups = func() []string { return []string{"work-context"} }
	builder := &Builder{
		instance: &coreservices.Instance{Service: &resources.Service{}},
		world:    world,
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
// manifest.
//
// The reference is asserted in full — the Secret it names, and the key inside
// it — because that is what the projected ExternalSecret has to match. "Some
// reference was produced" would pass for a reference naming the wrong Secret or
// a key no store could ever hold, which is the same outage as no reference at
// all, found later.
//
// The two cases are independent, not the same case twice: `authority-token` is
// credential-*named*, which is what promotes it with no declaration at all, and
// `opaque` carries Secret: true under a key no marker list recognizes, which is
// what proves the declared flag promotes on its own.
func TestACompositionRootGroupRendersItsCredentialsByReference(t *testing.T) {
	for _, test := range []struct {
		name     string
		secret   *basev0.ConfigurationValue
		storeKey string
	}{
		{
			"credential-named, no declaration",
			&basev0.ConfigurationValue{Key: "authority-token", Value: "plaintext"},
			"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__WORK_CONTEXT__AUTHORITY_TOKEN",
		},
		{
			"declared secret under a key no marker recognizes",
			&basev0.ConfigurationValue{Key: "opaque", Value: "plaintext", Secret: true},
			"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__WORK_CONTEXT__OPAQUE",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			render := rootGroupWorld(t,
				&basev0.ConfigurationValue{Key: "authority-url", Value: "https://authority.example"},
				test.secret,
			)
			require.Equal(t, []string{"work-context"}, groupSet(render),
				"the root group must reach the render at all")

			_, safe, references, err := promotableDeploymentConfigurations(&basev0.Configuration{}, render, "secret-api")
			require.NoError(t, err)

			// The exact reference, keyed by the environment variable the workload
			// reads: core's encoding of the group and the key
			// (resources.ConfigurationAsEnvironmentVariables), which is also the
			// name `deploy secrets` plans the store under.
			require.Equal(t, []string{test.storeKey}, slices.Sorted(maps.Keys(references)),
				"the root group's credential must render as a reference under the key the workload reads")
			// The same key, derived rather than written down: core's environment
			// encoding of the group and the key. pkg/deploysecrets derives it the
			// same way to plan the store under it, and the two packages cannot
			// import each other (pkg/gitops sits between them), so this is where
			// the halves are shown to be about one key.
			require.Equal(t, test.storeKey, workspaceSecretEnvironmentKey(t, "work-context", test.secret.GetKey()),
				"the rendered reference is no longer core's encoding of the group and the key")
			reference := references[test.storeKey]
			require.Equal(t, "secret-api", reference.GetName(), "the reference must name this service's own Secret")
			require.Equal(t, test.storeKey, reference.GetKey(), "the Secret key must be the one the ExternalSecret projects")

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
			_, inline := groupValue(safe, "work-context", test.secret.GetKey())
			require.False(t, inline, "a credential must not survive into the rendered tree")
		})
	}
}

// workspaceSecretEnvironmentKey is core's environment encoding of one secret
// value of one workspace configuration group — the name a workload reads it
// under, and the store key `deploy secrets` plans. It is the same call
// promotableConfiguration makes to name a reference.
func workspaceSecretEnvironmentKey(t *testing.T, group, key string) string {
	t.Helper()
	variables, err := resources.ConfigurationAsEnvironmentVariables(&basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name:                group,
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: key, Secret: true}},
		}},
	}, "", true)
	require.NoError(t, err)
	require.Len(t, variables, 1)
	return variables[0].Key
}

// A render whose root group references a producer of the deployment it cannot
// derive an address for refuses, rather than emitting a manifest with the value
// missing.
//
// This is the second half of the fix, and it is not redundant with producer
// discovery: discovery is what makes the address available, and this is what
// happens when it did not. Core resolves the root's run-wide groups leniently —
// it drops a value this consumer cannot resolve (#393) — so without this the
// render's own output is the only place the loss is visible, and only to whoever
// reads the committed manifest key by key. Verified: reverting discovery to the
// declared groups alone makes the parity render fail here by name instead of
// shipping `payments/worker` without its authority endpoint.
func TestARenderRefusesARootReferenceItCannotResolve(t *testing.T) {
	world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("work-context", "authority-endpoint", "${endpoint:platform/gateway/rest}"),
		},
		rootConfigs: []string{"work-context"},
	})
	world.Mode = SnapshotMode
	// The producer is part of the deployment, and nothing can derive its
	// address: no Dependencies, so discovery finds nothing to bind.
	world.setRunProducers([]string{"platform/gateway"}, nil)

	_, err := world.workspaceConfigurationsFor(context.Background(), &resources.Service{}, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "a render must not emit a manifest with a root group's value silently missing")
	require.Contains(t, err.Error(), "${endpoint:platform/gateway/rest}")
	require.Contains(t, err.Error(), "platform/gateway is part of this deployment")
}

// The same unresolved reference under `codefly run` is not refused: a producer
// the run contains but has not initialized has recorded no endpoint, and the
// graph orders a consumer after the producers of the groups it *declares* only
// (core's architecture.addConfigurationReferenceEdges reads
// WorkspaceConfigurationDependencies). A root group's reference therefore orders
// nothing, so refusing would fail a run whose consumer simply starts first.
//
// The run can deliver less than the render that way. It is the same direction a
// profile exclusion may, and the direction that cannot produce "works locally,
// unconfigured once deployed" — the fault cli#882 is about. Stated in
// docs/orchestration.md rather than left for someone to find.
func TestARunToleratesARootReferenceThatHasNotResolvedYet(t *testing.T) {
	world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("work-context", "authority-endpoint", "${endpoint:platform/gateway/rest}"),
		},
		rootConfigs: []string{"work-context"},
	})
	world.Mode = RunMode
	world.setRunProducers([]string{"platform/gateway"}, nil)

	confs, err := world.workspaceConfigurationsFor(context.Background(), &resources.Service{}, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err, "a run must not fail a consumer that starts before the producer of a root group's reference")
	_, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.False(t, delivered, "core drops the value it cannot resolve for this consumer")
}
