package orchestration

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
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
// both network managers.
//
// origins are the services the flow was invoked on. The run set is then DERIVED
// from them the way Flow.runClosure derives it — each origin plus its dependency
// closure — rather than declared by hand. That matters: a root group's reference
// adds no edge to that closure (core's
// ServiceDependencies.addConfigurationReferenceEdges reads declared groups
// only), so `codefly run payments/worker` really does run one service, and a
// hand-written run set of the whole workspace would hide what that does to a
// root reference. No origins means every service of the workspace, which is what
// a workspace-level run computes.
func parityWorld(t *testing.T, mode Mode, origins ...string) (*World, *resources.Workspace) {
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
	composedLoader := staticWorkspaceLoader{confs: []*basev0.Configuration{
		workspaceConfiguration("payments-store", "gateway-endpoint", "${endpoint:platform/gateway/rest}"),
	}}
	manager.WithLoader(composedLoader)
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
		// The same binding NewFlow makes from the same loader: the groups as
		// loaded, so a reference can be validated and a dropped value noticed.
		providedWorkspaceConfigurationInfos: func() []*basev0.ConfigurationInformation {
			return append(workspaceConfigurationInfos(localReader.Configurations()),
				workspaceConfigurationInfos(composedLoader.Configurations())...)
		},
	}
	runSet := parityRunClosure(t, dependencies, workspace, origins)
	world.setRunProducers(runSet, nil)

	// A local run derives a producer's address from the endpoints it recorded at
	// Load; a render derives it from the producer's identity and namespace. Only
	// the run needs the recording, so only the run world gets it — and only for
	// services actually in the run, since "it recorded endpoints at Load" is
	// exactly what run membership means to producerNetworkMappings.
	if !world.deploys() {
		for _, unique := range runSet {
			module, name, ok := strings.Cut(unique, "/")
			require.True(t, ok)
			service := parityService(t, workspace, module, name)
			identity, err := service.Identity()
			require.NoError(t, err)
			endpoints, err := service.LoadEndpoints(ctx)
			require.NoError(t, err)
			require.NoError(t, sharedState.RecordEndpoints(ctx, identity, endpoints))
		}
	}
	return world, workspace
}

// parityRunClosure is the run set a flow of these origins computes: each origin
// plus everything it depends on, exactly as Flow.runClosure walks it
// (Dependencies.OrderTo per root). With no origins it is every service of the
// workspace — the workspace-level run.
//
// It is derived rather than written down because the difference between the two
// is a real divergence this package has to be able to see: nothing adds an edge
// for a root group's reference, so a one-service run's closure does not contain
// the producer that group names.
func parityRunClosure(t *testing.T, dependencies *architecture.ServiceDependencies, workspace *resources.Workspace, origins []string) []string {
	t.Helper()
	ctx := context.Background()
	if len(origins) == 0 {
		services, err := workspace.LoadServices(ctx)
		require.NoError(t, err)
		for _, service := range services {
			identity, err := service.Identity()
			require.NoError(t, err)
			origins = append(origins, identity.Unique())
		}
	}
	seen := make(map[string]bool)
	var out []string
	add := func(unique string) {
		if seen[unique] {
			return
		}
		seen[unique] = true
		out = append(out, unique)
	}
	for _, origin := range origins {
		order, err := dependencies.OrderTo(ctx, origin)
		require.NoError(t, err)
		for _, service := range order {
			add(service.Unique)
		}
		add(origin)
	}
	slices.Sort(out)
	return out
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
		// withheld are the keys this service must NOT receive, in both paths.
		// `authority-token` is a credential in a group only platform/gateway
		// declares: a composition root's credentials reach the services that
		// ask for them, not every workload of the composition. Asserted in both
		// paths, because withholding in one of them would be the very asymmetry
		// this test exists to catch.
		withheld map[string][]string
	}{
		// The one declarer of `work-context`, so the one service the root's
		// credential reaches.
		{"platform", "gateway", []string{"work-context"},
			map[string][]string{"work-context": {"authority-url", "authority-endpoint", "authority-token"}}, nil},
		{"payments", "api", []string{"payments-store", "work-context"},
			map[string][]string{
				"work-context":   {"authority-url", "authority-endpoint"},
				"payments-store": {"gateway-endpoint"},
			},
			map[string][]string{"work-context": {"authority-token"}}},
		{"payments", "worker", []string{"work-context"},
			map[string][]string{"work-context": {"authority-url", "authority-endpoint"}},
			map[string][]string{"work-context": {"authority-token"}}},
		// The producer the root group references is itself a consumer of it: it
		// declares no group, and the reference it receives names its own
		// endpoint. Covered so no service of the fixture is left unasserted.
		{"platform", "authority", []string{"work-context"},
			map[string][]string{"work-context": {"authority-url", "authority-endpoint"}},
			map[string][]string{"work-context": {"authority-token"}}},
	} {
		expected, expectedKeys, withheldKeys := want.groups, want.keys, want.withheld
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

			// And the keys this service must not receive, in both paths. A
			// credential the root provides is not run-wide: it reaches the
			// services that declare its group. Both halves are asserted
			// because withholding it in the render alone would recreate #882
			// on the credential axis, and in the run alone would leave a
			// deployed workload holding a credential its own run never had.
			for group, keys := range withheldKeys {
				for _, key := range keys {
					_, inRun := groupValue(run, group, key)
					require.False(t, inRun, "the run delivered %s/%s to a service that does not declare %s", group, key, group)
					_, inRender := groupValue(render, group, key)
					require.False(t, inRender, "the render delivered %s/%s to a service that does not declare %s", group, key, group)
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

// requireInClusterAddress asserts a deployed address is EXACTLY the producer's
// in-cluster one: its Kubernetes service name, in its own namespace, at the port
// its manifest declares.
//
// Two weaker forms were here before and neither held. "Does not contain
// localhost" passes for 127.0.0.1, which is precisely the address a deployed
// workload cannot reach. A pattern whose namespace was "anything containing the
// module name" passes for a namespace belonging to another environment or
// another workspace — a deployed service reading a value that points into the
// wrong cell. The namespace is <workspace>-<module>-<environment>, and it is
// built here rather than matched loosely, so a change to the scheme fails this
// instead of silently widening what counts.
func requireInClusterAddress(t *testing.T, address, module, service string) {
	t.Helper()
	requireInClusterAddressIn(t, address, "group-parity", module, service, LocalEnvironmentName)
}

func requireInClusterAddressIn(t *testing.T, address, workspace, module, service, environment string) {
	t.Helper()
	host := service + "." + workspace + "-" + module + "-" + environment + ".svc.cluster.local"
	require.Regexp(t, regexp.MustCompile(`^(?:[a-z0-9-]+://)?`+regexp.QuoteMeta(host)+`:\d+$`),
		address, "a deployed consumer must reach %s/%s at exactly %s", module, service, host)
}

// addressPort is the port of an address, so a run-side assertion can say which
// endpoint an address belongs to rather than only that it is loopback.
func addressPort(t *testing.T, address string) string {
	t.Helper()
	index := strings.LastIndex(address, ":")
	require.Positive(t, index, "an address must carry a port: %q", address)
	return address[index+1:]
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

	// Which producer each loopback address belongs to, without re-deriving it.
	// `localhost:<port>` alone says nothing about whose port it is — the point
	// the review made — so each address is compared with the one the PRODUCER
	// itself resolves for the same key. platform/authority declares no group, so
	// it receives the root's `work-context` and resolves `authority-endpoint`
	// against its own endpoint; the port payments/api sees must be that one. And
	// the two references name different producers, so their ports must differ:
	// a single shared port would mean both resolved to whatever was bound last.
	authorityRunner := &Runner{
		instance:       parityInstance(t, workspace, parityService(t, workspace, "platform", "authority")),
		world:          runWorld,
		runtimeContext: resources.RuntimeContextNative,
	}
	fromProducer, err := authorityRunner.workspaceConfigurations(ctx, nil, runtimeContext)
	require.NoError(t, err)
	producerAddress, delivered := groupValue(fromProducer, "work-context", "authority-endpoint")
	require.True(t, delivered)
	consumerAddress, delivered := groupValue(run, "work-context", "authority-endpoint")
	require.True(t, delivered)
	require.Equal(t, addressPort(t, producerAddress), addressPort(t, consumerAddress),
		"the consumer must reach platform/authority on the port platform/authority itself resolves")
	storeAddress, delivered := groupValue(run, "payments-store", "gateway-endpoint")
	require.True(t, delivered)
	require.NotEqual(t, addressPort(t, storeAddress), addressPort(t, consumerAddress),
		"two references naming different producers must not resolve to one port")

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
func rootGroupWorld(t *testing.T, declared []string, values ...*basev0.ConfigurationValue) []*basev0.Configuration {
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
	world.providedWorkspaceConfigurationInfos = func() []*basev0.ConfigurationInformation {
		return []*basev0.ConfigurationInformation{{Name: "work-context", ConfigurationValues: values}}
	}
	builder := &Builder{
		instance: &coreservices.Instance{
			Service: &resources.Service{WorkspaceConfigurationDependencies: declared},
		},
		world: world,
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
// It reaches the services that DECLARE the group, which is the second half of
// this test and a deliberate limit on the first. A root group's non-secret
// values are run-wide; its credentials are not. Delivering them run-wide is what
// making the render resolve root groups would have done on its own, and it would
// have handed every workload of the composition every root credential as a
// mandatory secretKeyRef — one compromised service yielding all of them, and one
// copy per service in the environment's store. The run withholds them too, so
// the parity this PR is about is not broken by the narrowing: see
// withheldCredentials.
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
			render := rootGroupWorld(t, []string{"work-context"},
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
			// same way to plan the store under it. The halves do not share one
			// call because the promotion path here is unexported, not because of
			// the import graph (pkg/deploysecrets may import pkg/orchestration;
			// only the reverse would cycle), so this is where they are shown to
			// be about one key.
			require.Equal(t, test.storeKey, workspaceSecretEnvironmentKey(t, "work-context", test.secret.GetKey()),
				"the rendered reference is no longer core's encoding of the group and the key")
			reference := references[test.storeKey]
			require.Equal(t, "secret-api", reference.GetName(), "the reference must name this service's own Secret")
			require.Equal(t, test.storeKey, reference.GetKey(), "the Secret key must be the one the ExternalSecret projects")
			// Mandatory, not optional. An optional secretKeyRef lets the
			// workload start with the value simply absent, which is the #882
			// fault wearing a Kubernetes hat: the pod comes up, reads an empty
			// credential and fails at its first authenticated call. HEAD
			// already defaults this to false; asserting it is what stops a
			// later "make it tolerant" change from passing silently.
			require.False(t, reference.GetOptional(),
				"a credential reference must be mandatory: an optional one lets the workload start without the value")

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

			// The same group, the same render, for a service that does NOT
			// declare it: the group's non-secret value still arrives — it is
			// run-wide — and the credential is not there to promote, so no
			// reference is rendered and no store entry is planned for it.
			undeclared := rootGroupWorld(t, nil,
				&basev0.ConfigurationValue{Key: "authority-url", Value: "https://authority.example"},
				test.secret,
			)
			require.Equal(t, []string{"work-context"}, groupSet(undeclared),
				"the root group's non-credential values stay run-wide")
			_, hasCredential := groupValue(undeclared, "work-context", test.secret.GetKey())
			require.False(t, hasCredential,
				"a composition root's credential must not reach a service that does not declare its group")
			_, _, undeclaredReferences, err := promotableDeploymentConfigurations(&basev0.Configuration{}, undeclared, "secret-worker")
			require.NoError(t, err)
			require.Empty(t, undeclaredReferences,
				"no secretKeyRef may be rendered for a credential this service does not receive")
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

// The refusal and the tolerance that used to be pinned here are now
// TestARenderRefusesARootReferenceNoAddressCanBeDerivedFor and
// TestARunWithTemporaryPortsDropsARootReferenceItCannotPlaceYet, in
// root_group_reference_validity_test.go. They were replaced rather than moved:
// these two resolved over a World with no Dependencies and no SharedState, so
// producer discovery short-circuited before it began — which made the render
// test's own claim ("reverting discovery makes this fail by name") untrue of
// what it ran, and let the run test pass against the pre-change code. NewFlow
// always builds both, so the replacements do too, and the only thing missing
// from them is the thing each is about.

// The third asymmetry, and the one the parity test above cannot see because it
// runs the whole workspace: `codefly run payments/worker` runs ONE service.
//
// A root group's ${endpoint:…} adds no edge to the dependency closure (core's
// ServiceDependencies.addConfigurationReferenceEdges reads a consumer's declared
// groups and no more), so the producer that group names is not in a
// single-service run at all. A local address exists only for a service of the
// run, so the value is dropped — with a WARN naming it, not in silence. The
// render of the same one service derives the producer's address anyway, because
// a deployed address is a function of identity and namespace rather than of
// membership, so it delivers the value.
//
// So the run and the render do NOT always deliver the same keys, and the PR body
// and docs say so rather than claiming otherwise: the run delivers FEWER, which
// is the direction that cannot produce "works locally, unconfigured once
// deployed". A deployed service never loses a value because the operator ran one
// service locally.
func TestASingleServiceRunDropsARootReferenceTheRenderResolves(t *testing.T) {
	ctx := context.Background()
	runWorld, workspace := parityWorld(t, RunMode, "payments/worker")
	renderWorld, _ := parityWorld(t, SnapshotMode, "payments/worker")
	runtimeContext, err := resources.NewRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, err)

	require.Equal(t, []string{"payments/worker"}, parityRunClosure(t, runWorld.Dependencies, workspace, []string{"payments/worker"}),
		"the case is a run of one service; if the closure grows, the fixture gained an edge and this test is no longer about that")

	service := parityService(t, workspace, "payments", "worker")
	instance := parityInstance(t, workspace, service)

	runner := &Runner{instance: instance, world: runWorld, runtimeContext: resources.RuntimeContextNative}
	run, err := runner.workspaceConfigurations(ctx, nil, runtimeContext)
	require.NoError(t, err, "a one-service run must not fail over a root reference it cannot place")
	_, delivered := groupValue(run, "work-context", "authority-endpoint")
	require.False(t, delivered, "a one-service run has no address for a producer outside it")
	// The rest of the group still arrives: only the reference is lost.
	literal, delivered := groupValue(run, "work-context", "authority-url")
	require.True(t, delivered, "a literal in the same group is unaffected")
	require.Equal(t, "https://authority.example", literal)

	builder := &Builder{instance: instance, world: renderWorld}
	render, err := builder.workspaceConfigurations(ctx, nil)
	require.NoError(t, err)
	deployed, delivered := groupValue(render, "work-context", "authority-endpoint")
	require.True(t, delivered, "a render derives a deployed address without needing the producer in its run set")
	requireInClusterAddress(t, deployed, "platform", "authority")
}
