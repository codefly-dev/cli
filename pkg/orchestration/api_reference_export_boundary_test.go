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
