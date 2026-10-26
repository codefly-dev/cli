package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// A service whose own group references its own endpoint — the hop's ENDPOINT
// names the hop — reads the address its Init proposes, under
// --temporary-ports as well: that address is allocated by this Init, so it is
// known here, where deriving it as another producer's would allocate a second
// port and is refused.
func TestASelfReferenceResolvesToTheAddressThisInitProposes(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		t.Run(map[bool]string{false: "named-ports", true: "temporary-ports"}[temporary], func(t *testing.T) {
			workspace := writeTempWorkspace(t, map[string]string{
				"workspace.codefly.yaml":               "name: bare\nlayout: modules\nmodules:\n    - name: payments\n",
				"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nservices:\n    - name: worker\n",
				"configurations/local/hop.env":         "ENDPOINT=${endpoint:payments/worker/grpc}\n",
				"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
					"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
					"endpoints:\n    - name: grpc\n      api: grpc\n" +
					"workspace-configuration-dependencies:\n    - hop\n",
			})
			world, service := referenceValidityWorld(t, workspace, func(world *World) { world.Mode = RunMode })
			if temporary {
				world.temporaryPorts = true
				world.LocalNetworkManager.WithTemporaryPorts()
			}
			identity, err := service.Identity()
			require.NoError(t, err)
			endpoints, err := service.LoadEndpoints(t.Context())
			require.NoError(t, err)
			require.NoError(t, world.SharedState.RecordEndpoints(t.Context(), identity, endpoints))
			runner := &Runner{
				world:          world,
				instance:       &coreservices.Instance{Service: service, Identity: identity},
				runtimeContext: resources.RuntimeContextNative,
				endpoints:      endpoints,
			}
			inputs, err := runner.gatherInitInputs(t.Context(), t.Context(), wool.Get(t.Context()))
			require.NoError(t, err)
			value, found := groupValue(inputs.workspaceConfigurations, "hop", "ENDPOINT")
			require.True(t, found)
			proposed, err := resources.FindNetworkInstanceInNetworkMappings(t.Context(), inputs.networkMappings, endpoints[0], resources.NewNativeNetworkAccess())
			require.NoError(t, err)
			require.Equal(t, proposed.GetAddress(), value, "the reference reads the address this Init proposes")
		})
	}
}
