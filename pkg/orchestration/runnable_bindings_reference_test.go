package orchestration

import (
	"encoding/json"
	"testing"

	"github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// One runnable-bindings file whose call address is a reference, as `codefly
// generate runnable-bindings` writes it for a local call endpoint, is delivered
// under two naming scopes with the hop's address in each: the reference is
// resolved inside the JSON value, at the address that scope's run allocates.
func TestOneRunnableBindingsFileResolvesInEveryNamingScope(t *testing.T) {
	manifest := func(module, name, rest string) string {
		return "kind: service\nname: " + name + "\nversion: 0.0.0\nmodule: " + module + "\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" + rest
	}
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                                           "name: bare\nlayout: modules\nmodules:\n    - name: payments\n    - name: authority\n",
		"modules/payments/module.codefly.yaml":                             "kind: module\nname: payments\nservices:\n    - name: worker\n",
		"modules/authority/module.codefly.yaml":                            "kind: module\nname: authority\nservices:\n    - name: hop\n",
		"configurations/local/" + runnables.RunnableBindingsGroup + ".env": `DOCUMENTS__APPLY={"call":{"address":"${endpoint:authority/hop/http}","connect":{"procedure":"/documents.v1.Ingest/Apply"}}}` + "\n",
		// The worker offers the operation, which is what selects its binding
		// for delivery (runnables.PreparedSelectionFor).
		"configurations/local/" + runnables.OfferedToolsGroup + ".env": `PAYMENTS=["DOCUMENTS__APPLY"]` + "\n",
		"modules/payments/services/worker/service.codefly.yaml": manifest("payments", "worker",
			"workspace-configuration-dependencies:\n    - "+runnables.RunnableBindingsGroup+"\n    - "+runnables.OfferedToolsGroup+"\n"),
		"modules/authority/services/hop/service.codefly.yaml": manifest("authority", "hop",
			"endpoints:\n    - name: http\n      api: http\n      visibility: public\n      exposure: none\n"),
	})

	delivered := map[string]string{}
	for _, scope := range []string{"alpha", "beta"} {
		world, worker := referenceValidityWorld(t, workspace, func(world *World) {
			world.Mode = RunMode
			world.Env.NamingScope = scope
		})
		recordParityEndpoints(t, world, "authority", "hop")

		confs, err := world.workspaceConfigurationsFor(t.Context(), worker, nil, resources.NewNativeNetworkAccess())
		require.NoError(t, err)
		value, found := groupValue(confs, runnables.RunnableBindingsGroup, "DOCUMENTS__APPLY")
		require.True(t, found)
		var binding struct {
			Call struct {
				Address string `json:"address"`
			} `json:"call"`
		}
		require.NoError(t, json.Unmarshal([]byte(value), &binding), "the delivered value is still the JSON document")

		hop, err := world.producerNetworkMappings(t.Context(), "authority/hop")
		require.NoError(t, err)
		require.Len(t, hop, 1)
		instance, err := resources.FindNetworkInstanceInNetworkMappings(t.Context(), hop, hop[0].GetEndpoint(), resources.NewNativeNetworkAccess())
		require.NoError(t, err)
		require.Equal(t, instance.GetAddress(), binding.Call.Address, "scope %s delivers the hop's address in that scope", scope)
		require.Regexp(t, `^http://localhost:\d+$`, binding.Call.Address)
		delivered[scope] = binding.Call.Address
	}
	require.NotEqual(t, delivered["alpha"], delivered["beta"], "one file, two scopes, two addresses")
}
