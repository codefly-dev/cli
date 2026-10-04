package orchestration

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// apiReferenceWorkspace is a composition whose root group names an API rather
// than an endpoint, and whose producer declares TWO endpoints of that API: a
// public one first, then a private one. The consumer is in the other module and
// declares no group.
//
// The order is the fixture's whole point. Core's plan-time check
// (configurations.checkEndpointReference) walks the producer's manifest
// endpoints and returns on the FIRST one the reference matches — the public one
// — so the plan passes. Core's resolution
// (resources.resolveEndpointReference) walks the BOUND MAPPINGS instead, and
// takes the first one that matches and has an instance for the consumer's
// network access. The two walk different lists for different reasons, so an
// API-name reference can pass the check on the public endpoint and resolve to
// the private one.
func apiReferenceWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: api\n      api: rest\n      visibility: public\n" +
			"    - name: admin\n      api: rest\n      visibility: private\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		// An API-name reference: `rest` is the api of both endpoints above.
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:platform/authority/rest}\n",
	})
}

// recordMappings publishes a producer's network mappings on the shared state, in
// the order given, exactly as a producer's Init does. It is how both shapes
// below control what the resolution walks: a mapping's order and its instances
// come from the agent, not from the manifest.
func recordMappings(t *testing.T, world *World, module, name string, mappings ...*basev0.NetworkMapping) {
	t.Helper()
	ctx := context.Background()
	service, err := loadService(ctx, t, world.Workspace, module, name)
	require.NoError(t, err)
	require.NoError(t, world.SharedState.RecordNetworkMappings(ctx, service, mappings))
}

func endpointMapping(module, service, name, api, visibility string, instances ...*basev0.NetworkInstance) *basev0.NetworkMapping {
	return &basev0.NetworkMapping{
		Endpoint: &basev0.Endpoint{
			Module: module, Service: service, Name: name, Api: api, Visibility: visibility,
		},
		Instances: instances,
	}
}

func nativeInstance(address string) *basev0.NetworkInstance {
	return &basev0.NetworkInstance{Access: resources.NewNativeNetworkAccess(), Address: address}
}

func containerInstance(address string) *basev0.NetworkInstance {
	return &basev0.NetworkInstance{Access: resources.NewContainerNetworkAccess(), Address: address}
}

// An API-name reference in a composition-root group does not hand a consumer an
// address its module may not have.
//
// This is the layer-5 round-four blocker, in both shapes it was reproduced in.
// The hole was not in the check and not in core's resolution: it was in what
// this package BINDS. A root-referenced producer's mappings are bound for every
// service now, all of them, so a private sibling endpoint of the one the check
// approved was in the set the resolution walked — and the resolution's selection
// rule is not the check's. Nothing in between asked whether the consumer's
// module may reach the endpoint that actually won.
//
// Both shapes pass the plan gate and core's reference check, which is why
// neither was caught anywhere else: the reference IS legal, on the endpoint the
// check judged. Only the resolution's choice is wrong, so only the bound set can
// fix it here. See World.exportableTo.
func TestAnAPINameReferenceCannotResolveToAnEndpointTheConsumerMayNotReach(t *testing.T) {
	// The address the private endpoint would hand over. Asserted absent in both
	// shapes rather than only asserting the right one is present: "some address
	// arrived" passed for the wrong address before.
	const privateAddress = "http://localhost:2222"
	const publicAddress = "http://localhost:1111"

	t.Run("the private endpoint is bound first", func(t *testing.T) {
		world, service := referenceValidityWorld(t, apiReferenceWorkspace(t), func(world *World) {
			world.Mode = RunMode
			// Manifest order is public-then-private, so core's check passes on
			// `api`. The agent published them the other way round, which is the
			// order the resolution walks.
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "admin", "rest", "private", nativeInstance(privateAddress)),
				endpointMapping("platform", "authority", "api", "rest", "public", nativeInstance(publicAddress)),
			)
		})

		confs, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewNativeNetworkAccess())
		require.NoError(t, err, "the reference is legal on the public endpoint: the plan gate and core's check both pass it")
		address, delivered := groupValue(confs, "work-context", "authority-endpoint")
		require.True(t, delivered, "the value resolves — on the endpoint the consumer may reach")
		require.Equal(t, publicAddress, address,
			"an API-name reference must resolve to an endpoint this module may reach, whatever order the mappings arrive in")
		require.NotEqual(t, privateAddress, address)
	})

	t.Run("the public endpoint has no instance for this consumer's access", func(t *testing.T) {
		world, service := referenceValidityWorld(t, apiReferenceWorkspace(t), func(world *World) {
			world.Mode = RunMode
			// Manifest order is kept here. What makes the resolution fall
			// through is that `api` has only a container view — an agent may
			// drop a view it cannot serve (acceptNetworkInstances) — while this
			// consumer reads native addresses.
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "api", "rest", "public", containerInstance("http://api:8080")),
				endpointMapping("platform", "authority", "admin", "rest", "private", nativeInstance(privateAddress)),
			)
		})

		confs, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewNativeNetworkAccess())
		require.NoError(t, err, "a root group's unresolvable value is dropped in a run, not fatal")
		address, delivered := groupValue(confs, "work-context", "authority-endpoint")
		require.False(t, delivered,
			"with no reachable endpoint to resolve to, the value is dropped and warned about — never filled from a private sibling")
		require.NotEqual(t, privateAddress, address)
	})
}

// The same composition, with the consumer in the producer's OWN module, still
// resolves to the private endpoint when that is what the mappings offer.
//
// It is the boundary of the fix: resources.ValidateEndpointVisibility returns
// nil within one module, so the filter narrows nothing there. Without this a
// stricter rule — "never bind a private endpoint" — would pass the test above
// and silently break every service that reads its own module's endpoints.
func TestAPrivateEndpointStillResolvesForAConsumerInItsOwnModule(t *testing.T) {
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n    - name: sidecar\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: admin\n      api: rest\n      visibility: private\n",
		// Same module as the producer, declares no group: the root group reaches
		// it, and the producer's export boundary does not apply inside a module.
		"modules/platform/services/sidecar/service.codefly.yaml": "kind: service\nname: sidecar\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:platform/authority/admin}\n",
	})
	world, _ := referenceValidityWorld(t, workspace, func(world *World) {
		world.Mode = RunMode
		recordMappings(t, world, "platform", "authority",
			endpointMapping("platform", "authority", "admin", "rest", "private", nativeInstance("http://localhost:3333")),
		)
	})
	consumer, err := loadService(context.Background(), t, workspace, "platform", "sidecar")
	require.NoError(t, err)

	confs, err := world.workspaceConfigurationsFor(context.Background(), consumer, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	address, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.True(t, delivered, "a consumer in the producer's own module may read its private endpoint")
	require.Equal(t, "http://localhost:3333", address)
}

// A BARE service dependency is not a way around the export boundary either.
//
// This is the bypass the first revision of the filter left open, and it was the
// more reachable of the two: a dependency naming no endpoints is handed every
// mapping its producer published — StateManager.GetDependenciesNetworkMappings
// narrows by the dependency's endpoint list, never by visibility — and those
// mappings go straight into the set a ${endpoint:…} resolves against. Worse,
// producer discovery SKIPS a producer whose endpoint the dependency mappings
// already carry, so filtering only what discovery binds meant the filter never
// ran for exactly this consumer.
//
// The reference below names `rest`, which is `api`'s API and `rest`'s name, so
// it matches both of the producer's endpoints. Core's check passes on `api`
// (public, first in the manifest); the resolution walks the bound mappings,
// where the private one is first. (Layer-5 round-five NEW-1.)
func TestABareServiceDependencyIsNotAWayAroundTheExportBoundary(t *testing.T) {
	ctx := context.Background()
	const privateAddress = "http://localhost:2222"
	const publicAddress = "http://localhost:1111"

	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		// `api` first, so core's reference check passes on it.
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: api\n      api: rest\n      visibility: public\n" +
			"    - name: rest\n      api: grpc\n      visibility: private\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		// A bare dependency: the service, with no endpoint list. And no
		// workspace configuration group, so what it reads is the root's.
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"service-dependencies:\n    - name: authority\n      module: platform\n",
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:platform/authority/rest}\n",
	})

	world, service := referenceValidityWorld(t, workspace, func(world *World) {
		world.Mode = RunMode
		recordMappings(t, world, "platform", "authority",
			endpointMapping("platform", "authority", "rest", "grpc", "private", nativeInstance(privateAddress)),
			endpointMapping("platform", "authority", "api", "rest", "public", nativeInstance(publicAddress)),
		)
	})
	require.NotEmpty(t, service.ServiceDependencies, "the case is a declared dependency, not a discovered producer")

	// The mappings a real resolution is handed: the consumer's own dependency
	// mappings, as the shared state gives them.
	dependencyMappings, err := world.SharedState.GetDependenciesNetworkMappings(ctx, service)
	require.NoError(t, err)
	require.Len(t, dependencyMappings, 2, "a bare dependency is handed every endpoint its producer published")

	confs, err := world.workspaceConfigurationsFor(ctx, service, dependencyMappings, resources.NewNativeNetworkAccess())
	require.NoError(t, err, "the reference is legal on the public endpoint: core's check passes it")
	address, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.True(t, delivered, "the value resolves — on the endpoint the consumer may reach")
	require.Equal(t, publicAddress, address,
		"a declared dependency does not widen what a reference may resolve to")
	require.NotEqual(t, privateAddress, address)
}

// The producer's MANIFEST decides, not the mapping the agent published.
//
// In a run the mapping's endpoint is the agent's Load answer, recorded without
// being checked against the manifest. Judging visibility from it would make the
// export boundary a property of what a runtime agent says: an agent reporting a
// private endpoint as `public` would reopen the hole above, and one that dropped
// `allow-modules` would refuse a legitimate `internal` reference. Core's
// reference check reads the manifest; so does the filter.
//
// Below, the manifest says `admin` is private and the published mapping claims
// `public`. (Layer-5 round-five NEW-6.)
func TestTheProducersManifestDecidesVisibilityNotThePublishedMapping(t *testing.T) {
	ctx := context.Background()
	world, service := referenceValidityWorld(t,
		referenceValidityWorkspace(t, "platform/authority/admin", "private"),
		func(world *World) {
			world.Mode = RunMode
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "admin", "rest", "public",
					nativeInstance("http://localhost:4444")),
			)
		})

	// Core's check refuses the reference outright here, which is the first
	// guard; the mappings the agent published are the second, and the point is
	// that they cannot disagree with the manifest in the permissive direction.
	_, err := world.workspaceConfigurationsFor(ctx, service, nil, resources.NewNativeNetworkAccess())
	require.Error(t, err, "a private endpoint stays private however the agent reports it")
	require.Contains(t, err.Error(), "is private to module")

	// And directly, so the filter is exercised rather than only the check: the
	// mapping claims public, the manifest says private, and nothing is bound.
	mappings := []*basev0.NetworkMapping{
		endpointMapping("platform", "authority", "admin", "rest", "public", nativeInstance("http://localhost:4444")),
	}
	visible, err := world.exportableTo(ctx, service, mappings)
	require.NoError(t, err)
	require.Empty(t, visible, "the filter must read the manifest, not the mapping's own visibility")
}

// A mapping that does not say which endpoint it is does not get bound.
//
// The filter's verdict has to be about the endpoint whose address is in the
// mapping. An earlier revision fell back to matching a nameless mapping by API,
// which cannot establish that: with a public `api` and a private `admin` both on
// api `rest`, a mapping of `{Name: "", Api: "rest"}` carrying admin's address was
// judged against `api` — the first API match — approved as public, and core's
// interpolator then matched the retained mapping by API and returned admin's
// address. The visibility decision was about one endpoint and the address about
// another. (Layer-2 round-six finding 2.)
func TestAMappingWithNoEndpointNameIsNotBound(t *testing.T) {
	ctx := context.Background()
	const adminAddress = "http://localhost:2222"
	world, service := referenceValidityWorld(t, apiReferenceWorkspace(t), func(world *World) {
		world.Mode = RunMode
	})

	// Nameless, API-only, and the address is the private endpoint's.
	mappings := []*basev0.NetworkMapping{{
		Endpoint:  &basev0.Endpoint{Module: "platform", Service: "authority", Api: "rest"},
		Instances: []*basev0.NetworkInstance{nativeInstance(adminAddress)},
	}}
	visible, err := world.exportableTo(ctx, service, mappings)
	require.NoError(t, err)
	require.Empty(t, visible,
		"a mapping whose endpoint has no name cannot be judged, so it must not be bound")

	// And a mapping whose API contradicts the manifest endpoint of that name is
	// not bound either: the two fields would describe different endpoints.
	inconsistent := []*basev0.NetworkMapping{{
		Endpoint:  &basev0.Endpoint{Module: "platform", Service: "authority", Name: "api", Api: "grpc"},
		Instances: []*basev0.NetworkInstance{nativeInstance(adminAddress)},
	}}
	visible, err = world.exportableTo(ctx, service, inconsistent)
	require.NoError(t, err)
	require.Empty(t, visible, "a mapping must agree with the manifest about the endpoint it names")

	// The named, consistent mapping for the same endpoint is bound, so this is
	// about identity and not about rejecting the producer.
	named := []*basev0.NetworkMapping{{
		Endpoint:  &basev0.Endpoint{Module: "platform", Service: "authority", Name: "api", Api: "rest"},
		Instances: []*basev0.NetworkInstance{nativeInstance("http://localhost:1111")},
	}}
	visible, err = world.exportableTo(ctx, service, named)
	require.NoError(t, err)
	require.Len(t, visible, 1)
}

// A reference more than one permitted endpoint satisfies is refused by name.
//
// Core's trailing token matches an endpoint's name OR its API, so one reference
// can match several of a producer's endpoints; core judges the first and its
// interpolation takes the first bound mapping with an instance for the
// consumer's access. Filtering the bound set keeps that choice inside what the
// consumer may reach — it does not stop it choosing between two endpoints the
// consumer may both reach, and a value quietly addressing a different endpoint
// than its reference names is the fault this package exists to remove.
//
// The pair is the point: the same reference is unambiguous for a cross-module
// consumer, which may reach exactly one of the two, and ambiguous for one in the
// producer's own module, which may reach both.
func TestAReferenceSeveralPermittedEndpointsSatisfyIsRefused(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n    - name: sidecar\n",
		// Two endpoints a `rest` reference matches, BOTH by API and neither by
		// name: nothing in the reference picks one. (If either were named
		// `rest` the reference would name it exactly and resolve — see
		// TestAnExactEndpointNameWinsOverAnAPISibling.)
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: api\n      api: rest\n      visibility: public\n" +
			"    - name: admin\n      api: rest\n      visibility: private\n",
		// In the producer's module, so both endpoints are reachable for it.
		"modules/platform/services/sidecar/service.codefly.yaml": "kind: service\nname: sidecar\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:platform/authority/rest}\n",
	})
	world, worker := referenceValidityWorld(t, workspace, func(world *World) {
		world.Mode = RunMode
		recordMappings(t, world, "platform", "authority",
			endpointMapping("platform", "authority", "api", "rest", "public", nativeInstance("http://localhost:1111")),
			endpointMapping("platform", "authority", "admin", "rest", "private", nativeInstance("http://localhost:2222")),
		)
	})

	// Cross-module: only `api` is reachable, so the reference names one endpoint.
	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err, "one permitted match is not ambiguous")
	address, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:1111", address)

	// Same module: both are reachable, so the reference must be refused rather
	// than resolved to whichever is bound first.
	sidecar, err := loadService(ctx, t, workspace, "platform", "sidecar")
	require.NoError(t, err)
	_, err = world.workspaceConfigurationsFor(ctx, sidecar, nil, resources.NewNativeNetworkAccess())
	require.Error(t, err, "a reference two reachable endpoints satisfy must be refused")
	require.Contains(t, err.Error(), "could be any of several endpoints")
	require.Contains(t, err.Error(), "api")
	require.Contains(t, err.Error(), "admin")

	// And the PLAN GATE refuses it too, so an operator learns at plan time
	// rather than when the first service reads the value mid-run. The gate runs
	// the same rule over the groups each consumer receives.
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	err = PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{sidecar}, true)
	require.Error(t, err, "the gate must not pass a reference the resolution refuses")
	require.Contains(t, err.Error(), "could be any of several endpoints")
}

// The ambiguity refusal judges the consumer's own groups and nothing else.
//
// This is the regression the first revision of it shipped, and it broke the rule
// this package states everywhere else: a value a service does not receive
// imposes no obligation on it. The check was handed every information block the
// loader loaded — which includes groups a service never declared and the
// composition root does not provide run-wide — so one ambiguous reference
// anywhere in the workspace refused every consumer in it. (Layer-4 round-seven
// B1.)
//
// The pair is the test: the same ambiguous reference must not refuse a consumer
// that does not receive its group, and must refuse the one that does.
func TestAnAmbiguousReferenceOnlyRefusesTheConsumersThatReceiveIt(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		// Two endpoints a `rest` reference matches by API, neither by name, both
		// visible to every module: ambiguous for whoever receives the group.
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: api\n      api: rest\n      visibility: public\n" +
			"    - name: admin\n      api: rest\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n    - name: reader\n",
		// Declares a DIFFERENT group, so its effective set is non-empty and the
		// check really runs for it — `authority-pool` is simply not in it.
		// (With an empty effective set the check returns early and the test
		// would pass for the wrong reason: that is how the first version of
		// this fixture failed to catch the regression.)
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"workspace-configuration-dependencies:\n    - work-context\n",
		"modules/payments/services/reader/service.codefly.yaml": "kind: service\nname: reader\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"workspace-configuration-dependencies:\n    - authority-pool\n",
		"configurations/local/authority-pool.env": "authority-endpoint=${endpoint:platform/authority/rest}\n",
		"configurations/local/work-context.env":   "authority-url=https://authority.example\n",
	})

	// The group is composition-root by core's rule (nothing composed provides
	// it), so narrow the world to make this about the DECLARATION rather than
	// about the root set: excluding it leaves `worker` with no group at all and
	// `reader` with the one it declares.
	world, worker := referenceValidityWorld(t, workspace, func(world *World) {
		world.compositionRootGroups = func() []string { return nil }
	})

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err,
		"an ambiguous reference in a group this service does not receive must not refuse it")
	require.Equal(t, []string{"work-context"}, groupSet(confs),
		"the check must have run over a non-empty effective set, or this proves nothing")

	reader, err := loadService(ctx, t, workspace, "payments", "reader")
	require.NoError(t, err)
	_, err = world.workspaceConfigurationsFor(ctx, reader, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "the service that declares the group is held to the reference")
	require.Contains(t, err.Error(), "could be any of several endpoints")
	require.Contains(t, err.Error(), "authority-pool/authority-endpoint")
}

// An endpoint named exactly as the reference wins over a sibling that only
// matches its API.
//
// This is the over-refusal the ambiguity rule shipped with, and it refused
// ordinary compositions. A producer declaring `grpc` (api grpc) and
// `admin` (api grpc) makes `${endpoint:platform/authority/grpc}` match
// BOTH, because core's matcher is `Name == token || API == token` — and naming
// the api explicitly does not narrow the name branch. The reference says which
// endpoint it means, so refusing it makes a clear composition unresolvable.
//
// What stays refused is ambiguity among API-token matches, where nothing in the
// reference picks one: TestAReferenceSeveralPermittedEndpointsSatisfyIsRefused.
//
// Not closed here, and the reason the exemption is a decision rather than a
// detail: core's resolution does not prefer the exact name either — it takes the
// first bound mapping that matches — so in this shape the address can still come
// from the sibling. That is core's endpoint-identity gap, carried as a follow-up.
func TestAnExactEndpointNameWinsOverAnAPISibling(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		// `grpc` by name, `admin` by API: the reference matches both.
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: grpc\n      api: grpc\n      visibility: public\n" +
			"    - name: admin\n      api: grpc\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:platform/authority/grpc}\n",
	})
	world, worker := referenceValidityWorld(t, workspace, func(world *World) {
		world.Mode = RunMode
		recordMappings(t, world, "platform", "authority",
			endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("http://localhost:1111")),
			endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
		)
	})

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err,
		"a reference that names an endpoint exactly must resolve, whatever shares its API")
	address, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:1111", address, "and it resolves to the endpoint it names")

	// PRECEDENCE, not merely exemption: with the sibling published FIRST, and
	// again with the named endpoint carrying no instance for this consumer's
	// access, the reference must still never resolve to the sibling. Permitting
	// the reference without this would have traded a refusal of clear
	// compositions for a value silently addressing the wrong endpoint.
	for _, shape := range []struct {
		name     string
		mappings []*basev0.NetworkMapping
	}{
		{"the sibling is published first", []*basev0.NetworkMapping{
			endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
			endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("http://localhost:1111")),
		}},
		{"the named endpoint has no instance for this access", []*basev0.NetworkMapping{
			endpointMapping("platform", "authority", "grpc", "grpc", "public", containerInstance("http://grpc:9090")),
			endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ordered, service := referenceValidityWorld(t, workspace, func(world *World) {
				world.Mode = RunMode
				recordMappings(t, world, "platform", "authority", shape.mappings...)
			})
			confs, err := ordered.workspaceConfigurationsFor(ctx, service, nil, resources.NewNativeNetworkAccess())
			require.NoError(t, err)
			address, delivered := groupValue(confs, "work-context", "authority-endpoint")
			require.NotEqual(t, "http://localhost:2222", address,
				"the value must never address the sibling the reference did not name")
			if delivered {
				require.Equal(t, "http://localhost:1111", address)
			}
		})
	}

	// The same holds at the plan gate, which runs the same rule.
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	require.NoError(t, PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{worker}, true),
		"the gate must not refuse what the resolution resolves")
}

// Another producer's reference does not vouch for this producer's endpoint.
//
// `resources.EndpointMatchesReferenceInfo` compares an endpoint's API and its
// name and NEVER its module or service. So the precedence pass asked "does any
// reference legitimately want this mapping?" with a matcher that cannot tell one
// producer from another: a reference to `payments/ledger/grpc` — which has no
// exact-name answer of its own, so every API match is a legitimate candidate for
// it — vouched for `platform/authority/admin`, keeping it bound for the
// authority reference that names `grpc` exactly. Core's first match then writes
// admin's address into the value, with `err=nil` and a green plan gate: the exact
// mis-resolution precedence exists to stop, reintroduced by the fix for it.
// (Layer-5 round-seven N1.)
func TestAnotherProducersReferenceDoesNotKeepThisProducersSibling(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: grpc\n      api: grpc\n      visibility: public\n" +
			"    - name: admin\n      api: grpc\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n    - name: ledger\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		// A second producer whose only endpoint matches `grpc` by API, so a
		// reference to it has no exact-name answer — the shape that vouched.
		"modules/payments/services/ledger/service.codefly.yaml": "kind: service\nname: ledger\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: books\n      api: grpc\n      visibility: public\n",
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:platform/authority/grpc}\n" +
			"ledger-endpoint=${endpoint:payments/ledger/grpc}\n",
	})
	world, worker := referenceValidityWorld(t, workspace, func(world *World) {
		world.Mode = RunMode
		// authority's sibling first, so an unfiltered set resolves to it.
		recordMappings(t, world, "platform", "authority",
			endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
			endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("http://localhost:1111")),
		)
		recordMappings(t, world, "payments", "ledger",
			endpointMapping("payments", "ledger", "books", "grpc", "public", nativeInstance("http://localhost:3333")),
		)
	})

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	authority, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:1111", authority,
		"the exactly named endpoint wins, whatever another producer's reference matches")
	require.NotEqual(t, "http://localhost:2222", authority)
	// And the other producer's own reference still resolves, so the fix narrows
	// the vouching and not the set.
	ledger, delivered := groupValue(confs, "work-context", "ledger-endpoint")
	require.True(t, delivered, "a reference with no exact-name answer still resolves by API")
	require.Equal(t, "http://localhost:3333", ledger)
}

// Discovery binds the endpoint a reference NAMES even when the consumer's own
// dependency mappings already carry an API-sibling of it.
//
// mappingsCarry answered "the producer's endpoint is already here" for a mapping
// that merely shared the API, so discovery skipped the producer and the named
// endpoint's mapping was never bound. Precedence then stripped the sibling as
// the wrong answer, and the value was dropped — in a run with a WARN blaming the
// producer's address, which is the wrong cause, and refused outright in a
// render. Before precedence existed the same shape resolved silently to the
// sibling. (Layer-5 round-seven N2.)
func TestDiscoveryBindsTheNamedEndpointEvenWhenADependencyCarriesItsAPISibling(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: grpc\n      api: grpc\n      visibility: public\n" +
			"    - name: admin\n      api: grpc\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		// The consumer depends on `admin` specifically, so its dependency
		// mappings carry admin and nothing else.
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"service-dependencies:\n    - name: authority\n      module: platform\n      endpoints:\n          - name: admin\n",
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:platform/authority/grpc}\n",
	})
	world, worker := referenceValidityWorld(t, workspace, func(world *World) {
		world.Mode = RunMode
		recordMappings(t, world, "platform", "authority",
			endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("http://localhost:1111")),
			endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
		)
	})
	dependencyMappings, err := world.SharedState.GetDependenciesNetworkMappings(ctx, worker)
	require.NoError(t, err)
	require.Len(t, dependencyMappings, 1, "the declared dependency grants admin alone")
	require.Equal(t, "admin", dependencyMappings[0].GetEndpoint().GetName())

	confs, err := world.workspaceConfigurationsFor(ctx, worker, dependencyMappings, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	address, delivered := groupValue(confs, "work-context", "authority-endpoint")
	require.True(t, delivered,
		"the reference names grpc, so discovery must bind grpc rather than count admin as carrying it")
	require.Equal(t, "http://localhost:1111", address)
}

// An endpoint name core's schema refuses is refused by the render, so a fixture
// that uses one is not testing the render at all.
//
// `endpoint.proto` constrains an endpoint name to `^[a-z]+$`, 3 to 20
// characters — tighter than a module or a service, which allow digits and
// hyphens. An earlier revision of the precedence fixtures above used
// `grpc-admin`, which is not a legal endpoint name: it loads from YAML, because
// core's post-load check only looks for duplicates, and then fails the moment a
// render asks the network manager to derive addresses for it. So those fixtures
// exercised the run path and would have been silently useless in a render, and
// renaming them to `admin` was a correctness fix rather than cosmetics.
//
// This pins the rule itself, so the next fixture that reaches for a hyphen gets
// an answer here rather than a confusing failure three layers down. (Layer-5
// round-seven N4.)
func TestAnEndpointNameCoresSchemaRefusesFailsTheRender(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		// Hyphenated, so illegal — and accepted by the YAML load, which is what
		// makes it a trap.
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: grpc-admin\n      api: grpc\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": "authority-endpoint=${endpoint:platform/authority/grpc-admin}\n",
	})
	world, worker := referenceValidityWorld(t, workspace)
	require.True(t, world.deploys())

	_, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "a render cannot derive an address for an endpoint whose name core refuses")
	require.Contains(t, err.Error(), "cannot derive the addresses of platform/authority",
		"and it is the derivation that says so, with the producer named")
	require.Contains(t, err.Error(), "^[a-z]+$", "carrying core's own reason")
}

// twoReferenceWorkspace is one producer with two endpoints sharing an API, and a
// root group whose two values reference them BY NAME — `grpc` and `admin`. Both
// references are legal, both name their endpoint exactly, and both target the
// same producer.
func twoReferenceWorkspace(t *testing.T, values string) *resources.Workspace {
	t.Helper()
	return twoReferenceWorkspaceOrdered(t, values, false)
}

// twoReferenceWorkspaceOrdered is the same composition with the producer's two
// endpoints declared in either order.
//
// The manifest order is what a RENDER derives its mapping order from, so it is
// the only way to make a render's fixture unfavourable — and a favourable one
// passes with the ordering pass removed, which is what made the first render
// regression test non-discriminating. (Layer-4 round-ten B3.)
func twoReferenceWorkspaceOrdered(t *testing.T, values string, siblingFirst bool) *resources.Workspace {
	t.Helper()
	endpoints := "endpoints:\n    - name: grpc\n      api: grpc\n      visibility: public\n" +
		"    - name: admin\n      api: grpc\n      visibility: public\n"
	if siblingFirst {
		endpoints = "endpoints:\n    - name: admin\n      api: grpc\n      visibility: public\n" +
			"    - name: grpc\n      api: grpc\n      visibility: public\n"
	}
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			endpoints,
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": values,
	})
}

// Two references into ONE producer each resolve to the endpoint they name.
//
// Removing the wrong answers is per MAPPING; core's selection is per REFERENCE
// against the whole retained list. So two references into one producer defeated
// removal on its own: a reference naming `grpc` and another naming `admin` each
// keep their own endpoint, and core then resolved the FIRST reference against
// both — first match wins, and `admin` (api grpc) answers a `grpc` reference.
// The value addressed an endpoint its reference did not name, with no error, and
// the outcome guard saw the key present. (Layer-4 round-eight R8-1.)
//
// The single-reference control is kept deliberately: with `secondary-address`
// removed, `admin` is stripped as a wrong answer and the old code passed. It is
// the second reference that makes it fail, which is why no earlier test caught
// it.
func TestTwoReferencesIntoOneProducerEachResolveToTheEndpointTheyName(t *testing.T) {
	ctx := context.Background()
	const bothValues = "primary-address=${endpoint:platform/authority/grpc}\n" +
		"secondary-address=${endpoint:platform/authority/admin}\n"

	// The mapping order that makes the wrong answer win: `admin` first.
	reversed := []*basev0.NetworkMapping{
		endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
		endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("http://localhost:1111")),
	}

	t.Run("two references, reversed mapping order", func(t *testing.T) {
		world, worker := referenceValidityWorld(t, twoReferenceWorkspace(t, bothValues), func(world *World) {
			world.Mode = RunMode
			recordMappings(t, world, "platform", "authority", reversed...)
		})

		confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
		require.NoError(t, err)
		primary, delivered := groupValue(confs, "work-context", "primary-address")
		require.True(t, delivered)
		require.Equal(t, "http://localhost:1111", primary,
			"the reference naming grpc must resolve to grpc even though admin shares its API and is bound first")
		secondary, delivered := groupValue(confs, "work-context", "secondary-address")
		require.True(t, delivered)
		require.Equal(t, "http://localhost:2222", secondary,
			"and the reference naming admin still gets admin")
	})

	// The discriminating control: one reference, same producer, same order.
	t.Run("one reference is the control", func(t *testing.T) {
		world, worker := referenceValidityWorld(t,
			twoReferenceWorkspace(t, "primary-address=${endpoint:platform/authority/grpc}\n"),
			func(world *World) {
				world.Mode = RunMode
				recordMappings(t, world, "platform", "authority", reversed...)
			})

		confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
		require.NoError(t, err)
		primary, delivered := groupValue(confs, "work-context", "primary-address")
		require.True(t, delivered)
		require.Equal(t, "http://localhost:1111", primary)
	})

	// A render counterpart, where the addresses are derived rather than
	// published, so the same selection runs over the deploy path.
	t.Run("the named endpoint has no address for this access", func(t *testing.T) {
		world, worker := referenceValidityWorld(t, twoReferenceWorkspace(t, bothValues), func(world *World) {
			world.Mode = RunMode
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
				endpointMapping("platform", "authority", "grpc", "grpc", "public", containerInstance("http://grpc:9090")),
			)
		})

		_, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
		require.Error(t, err,
			"a reference whose named endpoint has no address for this access must be refused, not answered by a sibling")
		require.Contains(t, err.Error(), "no address for")
		require.Contains(t, err.Error(), "grpc")
		require.NotContains(t, err.Error(), "localhost:2222")
	})

	for _, order := range []struct {
		name         string
		siblingFirst bool
	}{
		{"manifest order", false},
		// The unfavourable one: a render derives its mappings in manifest
		// order, so with the sibling declared FIRST an unordered set hands it
		// to the reference that names the other endpoint. The favourable order
		// passes with the ordering pass removed, which is why both are here.
		{"sibling declared first", true},
	} {
		t.Run("a render resolves each reference to its own endpoint, "+order.name, func(t *testing.T) {
			requireRenderAnswersEachReference(t, order.siblingFirst)
		})
	}
}

func requireRenderAnswersEachReference(t *testing.T, siblingFirst bool) {
	t.Helper()
	ctx := context.Background()
	const bothValues = "primary-address=${endpoint:platform/authority/grpc}\n" +
		"secondary-address=${endpoint:platform/authority/admin}\n"
	{
		world, worker := referenceValidityWorld(t, twoReferenceWorkspaceOrdered(t, bothValues, siblingFirst))
		require.True(t, world.deploys())

		confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
		require.NoError(t, err)
		primary, delivered := groupValue(confs, "work-context", "primary-address")
		require.True(t, delivered)
		secondary, delivered := groupValue(confs, "work-context", "secondary-address")
		require.True(t, delivered)
		require.NotEqual(t, primary, secondary,
			"two references naming different endpoints must not resolve to one address")

		// The discriminating assertion, because a render derives its addresses
		// rather than taking them from a published mapping: the SAME reference,
		// resolved in a render that has no second reference to keep the sibling
		// bound, must give the same address. If the second reference can shift
		// which endpoint answers the first, these differ — and asserting only
		// that the two values differ from each other would not see it.
		alone, aloneWorker := referenceValidityWorld(t,
			twoReferenceWorkspaceOrdered(t, "primary-address=${endpoint:platform/authority/grpc}\n", siblingFirst))
		aloneConfs, err := alone.workspaceConfigurationsFor(ctx, aloneWorker, nil, resources.NewContainerNetworkAccess())
		require.NoError(t, err)
		aloneAddress, delivered := groupValue(aloneConfs, "work-context", "primary-address")
		require.True(t, delivered)
		require.Equal(t, aloneAddress, primary,
			"the endpoint answering a reference must not depend on what else references its producer")
	}
}

// References whose orders contradict each other are refused rather than one of
// them silently losing.
//
// Endpoints (name `grpc`, api `rest`) and (name `rest`, api `grpc`) referenced
// by both names need opposite orders in the one list core scans: `grpc` first to
// answer `${…/grpc}`, `rest` first to answer `${…/rest}`. No ordering serves
// both and removal cannot help, because each endpoint is the exact answer to one
// of the references. Refusing names the pair; the alternative is one value
// addressing the other's endpoint.
func TestReferencesThatNeedOppositeOrdersAreRefused(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		// Each endpoint's name is the other's API.
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: grpc\n      api: rest\n      visibility: public\n" +
			"    - name: rest\n      api: grpc\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": "one=${endpoint:platform/authority/grpc}\n" +
			"two=${endpoint:platform/authority/rest}\n",
	})
	world, worker := referenceValidityWorld(t, workspace, func(world *World) {
		world.Mode = RunMode
		recordMappings(t, world, "platform", "authority",
			endpointMapping("platform", "authority", "grpc", "rest", "public", nativeInstance("http://localhost:1111")),
			endpointMapping("platform", "authority", "rest", "grpc", "public", nativeInstance("http://localhost:2222")),
		)
	})

	_, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	require.Error(t, err, "no single mapping order answers both references, so neither is guessed")
	require.Contains(t, err.Error(), "cannot all be answered from one set of network mappings")
	require.Contains(t, err.Error(), "platform/authority")
}

// A reference whose named endpoint published NO mapping is answered by nothing,
// never by the sibling that shares its API.
//
// This is the hole the first precedence pass left. Removal only fired when the
// sibling was the *only* wrong answer; with a second reference legitimately
// needing that sibling it stayed bound, and then — if the named endpoint never
// published a mapping at all — core's first match handed its address to the
// reference that named the absent one. `err=nil`, and the outcome guard saw the
// key present.
//
// The answer is that the siblings are dropped for that producer, so the
// reference resolves to nothing: a WARN drop in a run, refused by the outcome
// check in a render. A reference that cannot be answered correctly is answered
// not at all — including for the innocent second reference, which is the price
// of one shared mapping list and is visible rather than silent.
// (Layer-4 round-nine R9-1.)
func TestAReferenceWhoseNamedEndpointPublishedNoMappingIsNotAnsweredByASibling(t *testing.T) {
	ctx := context.Background()
	const bothValues = "primary-address=${endpoint:platform/authority/grpc}\n" +
		"secondary-address=${endpoint:platform/authority/admin}\n"

	t.Run("a run drops it", func(t *testing.T) {
		world, worker := referenceValidityWorld(t, twoReferenceWorkspace(t, bothValues), func(world *World) {
			world.Mode = RunMode
			// `grpc` published nothing; only its API sibling did.
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
			)
		})

		confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
		require.NoError(t, err, "a run drops what it cannot place")
		primary, delivered := groupValue(confs, "work-context", "primary-address")
		require.False(t, delivered,
			"the reference names an endpoint that published no mapping, so it resolves to nothing")
		require.NotEqual(t, "http://localhost:2222", primary, "and never to the sibling")
	})

	t.Run("a render refuses it", func(t *testing.T) {
		world, worker := referenceValidityWorld(t, twoReferenceWorkspace(t, bothValues), func(world *World) {
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "admin", "grpc", "public", containerInstance("http://admin:9090")),
			)
		})
		require.True(t, world.deploys())

		_, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
		require.Error(t, err, "a render must not emit a manifest with the wrong endpoint's address or a missing value")
		require.Contains(t, err.Error(), "work-context/primary-address")
	})
}

// Two mappings for one endpoint name do not look like an ordering cycle.
//
// The constraint sort compared the number of ordered names against the input
// list, and a producer that published the same endpoint twice made that list
// longer than the set — so a composition with nothing wrong with it was refused
// as unanswerable. Names are deduplicated before ordering now. (Layer-4
// round-nine R9-2.)
func TestDuplicateMappingsForOneEndpointAreNotACycle(t *testing.T) {
	ctx := context.Background()
	world, worker := referenceValidityWorld(t,
		twoReferenceWorkspace(t, "primary-address=${endpoint:platform/authority/grpc}\n"+
			"secondary-address=${endpoint:platform/authority/admin}\n"),
		func(world *World) {
			world.Mode = RunMode
			// The SIBLING is published twice, which is the shape that made the
			// sort miscount: it is the endpoint carrying an ordering
			// constraint, so the duplicate is never re-queued and the ordered
			// list came out shorter than the name list.
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("http://localhost:1111")),
				endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
				endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
			)
		})

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err, "a duplicate mapping is not a contradiction between references")
	primary, delivered := groupValue(confs, "work-context", "primary-address")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:1111", primary)
	secondary, delivered := groupValue(confs, "work-context", "secondary-address")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:2222", secondary)
}

// Two references over DISTINCT APIs resolve, and are never refused for an
// access a sibling could not have answered anyway.
//
// The access refusal fired whenever a reference's named endpoint lacked an
// address for the consumer's access and anything else matched that reference —
// including when no sibling had an address for that access either, so nothing
// could have been handed over wrongly and there was nothing to refuse. It is
// narrowed to the case where a sibling actually serves the access. (Layer-4
// round-nine R9-3.)
func TestTwoReferencesOverDistinctAPIsResolve(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		// Distinct APIs: neither reference matches the other's endpoint.
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: grpc\n      api: grpc\n      visibility: public\n" +
			"    - name: rest\n      api: rest\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": "grpc-address=${endpoint:platform/authority/grpc}\n" +
			"rest-address=${endpoint:platform/authority/rest}\n",
	})
	world, worker := referenceValidityWorld(t, workspace, func(world *World) {
		world.Mode = RunMode
		recordMappings(t, world, "platform", "authority",
			endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("http://localhost:1111")),
			endpointMapping("platform", "authority", "rest", "rest", "public", nativeInstance("http://localhost:3333")),
		)
	})

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err, "two references over distinct APIs cannot answer each other, so neither is refused")
	grpcAddress, delivered := groupValue(confs, "work-context", "grpc-address")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:1111", grpcAddress)
	restAddress, delivered := groupValue(confs, "work-context", "rest-address")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:3333", restAddress)

	// And the narrowing itself: the named endpoint has no address for this
	// consumer's access AND neither does the sibling that matches the same
	// reference. Nothing could be handed over wrongly, so there is nothing to
	// refuse — the value drops. Refusing here rejected reference sets that
	// could not possibly return a sibling's address.
	t.Run("no address for this access anywhere is a drop, not a refusal", func(t *testing.T) {
		world, worker := referenceValidityWorld(t,
			twoReferenceWorkspace(t, "primary-address=${endpoint:platform/authority/grpc}\n"+
				"secondary-address=${endpoint:platform/authority/admin}\n"),
			func(world *World) {
				world.Mode = RunMode
				recordMappings(t, world, "platform", "authority",
					endpointMapping("platform", "authority", "grpc", "grpc", "public", containerInstance("http://grpc:9090")),
					endpointMapping("platform", "authority", "admin", "grpc", "public", containerInstance("http://admin:9090")),
				)
			})

		confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
		require.NoError(t, err,
			"neither the named endpoint nor the sibling serves this access, so there is nothing to refuse")
		_, delivered := groupValue(confs, "work-context", "primary-address")
		require.False(t, delivered, "the value drops instead")
	})
}

// Access is judged over EVERY mapping an endpoint published, not the last one.
//
// A producer may publish more than one mapping for one endpoint — two instances
// recorded separately, a re-publish — and core scans them all. The ordering
// pass kept only the LAST matching mapping as `named` and asked that one
// whether it served the consumer's access, so a set whose EARLIER mapping
// carried the access read as "the named endpoint cannot answer" and the whole
// reference was refused. Nothing is wrong with this composition: `grpc` has a
// native address and the reference naming it must get it. (Layer-4 round-ten
// B1.)
//
// Both references are needed for the misread to be reachable: with only the one
// naming `grpc`, the sibling is removed as a wrong answer before the ordering
// pass runs and there is no sibling left to prefer.
func TestAnEarlierMappingOfTheNamedEndpointCarriesTheAccess(t *testing.T) {
	ctx := context.Background()
	world, worker := referenceValidityWorld(t,
		twoReferenceWorkspace(t, "primary-address=${endpoint:platform/authority/grpc}\n"+
			"secondary-address=${endpoint:platform/authority/admin}\n"),
		func(world *World) {
			world.Mode = RunMode
			recordMappings(t, world, "platform", "authority",
				// The access this consumer needs is on the FIRST of the two
				// mappings for `grpc`; the second carries only the other one.
				endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("http://localhost:1111")),
				endpointMapping("platform", "authority", "grpc", "grpc", "public", containerInstance("http://grpc:9090")),
				endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
			)
		})

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err,
		"the named endpoint does serve this access, on its first mapping: there is nothing to refuse")
	primary, delivered := groupValue(confs, "work-context", "primary-address")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:1111", primary,
		"and it resolves to the named endpoint's own address, not the sibling's")
	secondary, delivered := groupValue(confs, "work-context", "secondary-address")
	require.True(t, delivered)
	require.Equal(t, "http://localhost:2222", secondary)
}

// An endpoint that answers with an EMPTY address has answered: it is not absent.
//
// resources.resolveEndpointReference stops at the first instance whose access
// matches and, if its address is empty, returns an error naming that reference —
// it does not continue to the next match. Treating an empty address as absence
// made the ordering pass model a fall-through core will not perform, and refuse
// a reference set over a sibling core could never have reached. The honest
// outcome is the one core produces for the endpoint the reference names, with no
// mention of the sibling. (Layer-4 round-ten B2.)
func TestAnEmptyAddressOnTheNamedEndpointIsNotAbsence(t *testing.T) {
	ctx := context.Background()
	world, worker := referenceValidityWorld(t,
		twoReferenceWorkspace(t, "primary-address=${endpoint:platform/authority/grpc}\n"+
			"secondary-address=${endpoint:platform/authority/admin}\n"),
		func(world *World) {
			world.Mode = RunMode
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "grpc", "grpc", "public", nativeInstance("")),
				endpointMapping("platform", "authority", "admin", "grpc", "public", nativeInstance("http://localhost:2222")),
			)
		})

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	if err != nil {
		require.NotContains(t, err.Error(), "also satisfies that reference",
			"the ordering pass must not refuse over a sibling core cannot fall through to")
		require.NotContains(t, err.Error(), "localhost:2222")
		return
	}
	primary, delivered := groupValue(confs, "work-context", "primary-address")
	require.NotEqual(t, "http://localhost:2222", primary,
		"the sibling must never answer a reference that named the other endpoint")
	require.False(t, delivered && primary != "",
		"the named endpoint answered with nothing, so the value drops")
}
