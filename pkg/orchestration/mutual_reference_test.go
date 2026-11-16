package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// Two services whose groups reference each other's endpoints — a hop whose
// routing names its upstream, and the upstream whose group names the hop —
// each read the address the other's Init proposes, under --temporary-ports as
// under named ports. Neither can be ordered before the other, so the first
// Init derives the second's address before it initializes; that address is
// the one the second's own Init then proposes.
func TestMutualReferencesResolveToTheAddressEachInitProposes(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		t.Run(map[bool]string{false: "named-ports", true: "temporary-ports"}[temporary], func(t *testing.T) {
			manifest := func(module, name, group string) string {
				return "kind: service\nname: " + name + "\nversion: 0.0.0\nmodule: " + module + "\n" +
					"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
					"endpoints:\n    - name: grpc\n      api: grpc\n      visibility: public\n      exposure: none\n" +
					"workspace-configuration-dependencies:\n    - " + group + "\n"
			}
			workspace := writeTempWorkspace(t, map[string]string{
				"workspace.codefly.yaml":                                "name: bare\nlayout: modules\nmodules:\n    - name: payments\n    - name: authority\n",
				"modules/payments/module.codefly.yaml":                  "kind: module\nname: payments\nservices:\n    - name: worker\n",
				"modules/authority/module.codefly.yaml":                 "kind: module\nname: authority\nservices:\n    - name: hop\n",
				"configurations/local/hop.env":                          "UPSTREAM=${endpoint:payments/worker/grpc}\n",
				"configurations/local/caller.env":                       "HOP=${endpoint:authority/hop/grpc}\n",
				"modules/payments/services/worker/service.codefly.yaml": manifest("payments", "worker", "caller"),
				"modules/authority/services/hop/service.codefly.yaml":   manifest("authority", "hop", "hop"),
			})
			world, worker := referenceValidityWorld(t, workspace, func(world *World) { world.Mode = RunMode })
			if temporary {
				world.temporaryPorts = true
				world.LocalNetworkManager.WithTemporaryPorts()
			}
			hop, err := loadService(t.Context(), t, workspace, "authority", "hop")
			require.NoError(t, err)

			// Every service of a run loads, and records its endpoints, before
			// any initializes.
			runners := map[string]*Runner{}
			for _, service := range []*resources.Service{worker, hop} {
				identity, err := service.Identity()
				require.NoError(t, err)
				endpoints, err := service.LoadEndpoints(t.Context())
				require.NoError(t, err)
				require.NoError(t, world.SharedState.RecordEndpoints(t.Context(), identity, endpoints))
				runners[identity.Unique()] = &Runner{
					world:          world,
					instance:       &coreservices.Instance{Service: service, Identity: identity},
					runtimeContext: resources.RuntimeContextNative,
					endpoints:      endpoints,
				}
			}
			proposed := func(unique string, inputs *runnerInitInputs) string {
				instance, err := resources.FindNetworkInstanceInNetworkMappings(t.Context(), inputs.networkMappings, runners[unique].endpoints[0], resources.NewNativeNetworkAccess())
				require.NoError(t, err)
				return instance.GetAddress()
			}

			workerInputs, err := runners["payments/worker"].gatherInitInputs(t.Context(), t.Context(), wool.Get(t.Context()))
			require.NoError(t, err, "the first Init reads a reference to a service that has not initialized")
			hopInputs, err := runners["authority/hop"].gatherInitInputs(t.Context(), t.Context(), wool.Get(t.Context()))
			require.NoError(t, err)

			hopAddress, found := groupValue(workerInputs.workspaceConfigurations, "caller", "HOP")
			require.True(t, found)
			require.Equal(t, proposed("authority/hop", hopInputs), hopAddress, "the worker reads the address the hop's Init proposes")
			workerAddress, found := groupValue(hopInputs.workspaceConfigurations, "hop", "UPSTREAM")
			require.True(t, found)
			require.Equal(t, proposed("payments/worker", workerInputs), workerAddress, "the hop reads the address the worker's Init proposed")
		})
	}
}
