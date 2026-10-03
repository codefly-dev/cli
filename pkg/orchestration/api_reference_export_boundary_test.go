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
