package orchestration

import (
	"context"
	"fmt"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The MODULE's interface entry decides who may reach an endpoint, not the
// service's own declaration — in both directions.
//
// core#711 made the export boundary derived: Module.applyInterface stamps each
// endpoint with the visibility the module's interface exports it at, keeping
// the service's authored values aside, and core calls that "the single place
// the boundary is applied". Since core v0.14.0 (core#717) an interface entry
// states reach only — `internal` (any module of the composition) or `public`
// — and names nobody: narrowing is omitting the endpoint from the interface,
// which keeps it private, and an authored allow-list is refused by key. The
// CLI reaches the derived declaration by loading producers through the
// workspace — Workspace.LoadServices → LoadService →
// Module.LoadServiceFromName → loadServiceFromReference → applyInterface — so
// what World.exportableTo judges is already the module's export.
//
// Nothing asserted that. The CLI's export-boundary tests build producer
// declarations by hand, so every one of them would pass against a producer
// loaded WITHOUT its module, which is the regression this guards: a future
// change that reads services directly would silently judge consumers against
// the service's authored visibility again, and the authored value is exactly
// what core#711 says must not decide a consumer's reach.
//
// The fixture makes the two disagree on purpose, so neither case can pass by
// coincidence: the service authors PUBLIC while the module's interface omits
// the endpoint (exporting only a sibling), and the service authors PRIVATE
// while the module exports it publicly, or internally to every module of the
// composition.
func TestTheModuleInterfaceDecidesReachAndNotTheService(t *testing.T) {
	for _, test := range []struct {
		name              string
		authored          string
		interfaceEntry    string
		expectReachable   bool
		becauseTheService string
	}{
		{
			name:     "the module narrows what the service published",
			authored: "public",
			// The interface exports `admin` and omits `api`, which keeps `api`
			// private: omission is how a module narrows, since an entry can
			// only say internal or public and never name who.
			interfaceEntry:    "endpoint: admin\n      visibility: internal\n",
			expectReachable:   false,
			becauseTheService: "authored public, which must not win",
		},
		{
			name:              "the module widens what the service kept private",
			authored:          "private",
			interfaceEntry:    "endpoint: api\n      visibility: public\n",
			expectReachable:   true,
			becauseTheService: "authored private, which must not win either",
		},
		{
			name:              "the module exports it internal, naming nobody",
			authored:          "private",
			interfaceEntry:    "endpoint: api\n      visibility: internal\n",
			expectReachable:   true,
			becauseTheService: "authored private; internal permits every module of the composition",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := writeTempWorkspace(t, map[string]string{
				"workspace.codefly.yaml": "name: derivation\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
				// The module's interface IS the export declaration.
				"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: derivation\n" +
					"domain: github.com/codefly-ai/derivation/platform\n" +
					"interface:\n  endpoints:\n    - service: authority\n      " + test.interfaceEntry +
					"services:\n    - name: authority\n",
				// `admin` is a sibling the interface can export instead of `api`;
				// no mapping below carries it.
				"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
					"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
					"endpoints:\n    - name: api\n      api: rest\n      visibility: " + test.authored + "\n" + statedExposure(test.authored) +
					"    - name: admin\n      api: rest\n",
				"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: derivation\n" +
					"domain: github.com/codefly-ai/derivation/payments\nservices:\n    - name: worker\n",
				"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
					"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
			})

			// The consumer is loaded too, so its identity is the module's and
			// not a value this test chose.
			consumer, err := workspace.LoadService(context.Background(), &resources.ServiceWithModule{
				Name: "worker", Module: "payments",
			})
			require.NoError(t, err)
			identity, err := consumer.Identity()
			require.NoError(t, err)
			require.Equal(t, "payments", identity.Module)

			world := &World{Workspace: workspace}
			mappings := []*basev0.NetworkMapping{{
				Endpoint: &basev0.Endpoint{
					Module: "platform", Service: "authority", Name: "api", Api: "rest",
				},
				Instances: []*basev0.NetworkInstance{{
					Address: "localhost:14311", Access: resources.NewNativeNetworkAccess(),
				}},
			}}

			exportable, err := world.exportableTo(context.Background(), consumer, mappings)
			require.NoError(t, err)
			if test.expectReachable {
				require.Len(t, exportable, 1,
					fmt.Sprintf("the module exported it to payments; the service %s", test.becauseTheService))
				return
			}
			require.Empty(t, exportable,
				fmt.Sprintf("the module did not export it to payments; the service %s", test.becauseTheService))
		})
	}
}

// And the authored value is still what the service's own file says, so the
// derivation is an export boundary rather than a rewrite of the manifest.
//
// This matters for save: core keeps the authored export aside and restores it
// in preSave, so a module interface must not silently rewrite the service file
// a human maintains.
func TestTheDerivationLeavesTheServiceFileAuthoredAsWritten(t *testing.T) {
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: derivation\nlayout: modules\nmodules:\n    - name: platform\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: derivation\n" +
			"domain: github.com/codefly-ai/derivation/platform\n" +
			"interface:\n  endpoints:\n    - service: authority\n      endpoint: api\n      visibility: public\n" +
			"services:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: api\n      api: rest\n      visibility: private\n",
	})
	service, err := workspace.LoadService(context.Background(), &resources.ServiceWithModule{
		Name: "authority", Module: "platform",
	})
	require.NoError(t, err)
	require.Len(t, service.Endpoints, 1)
	// The LOADED endpoint carries the module's export: that is what every
	// reader of visibility observes, including this package's filter.
	require.Equal(t, resources.VisibilityPublic, service.Endpoints[0].Visibility,
		"the loaded endpoint must carry the module's export, not the service's authored value")
}
