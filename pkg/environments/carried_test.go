package environments_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// carriedWorkspace declares every carried key on a deployed environment beside
// a local one that declares none, the shape a composition writes.
const carriedWorkspace = `name: product
environments:
  - name: local
    cluster:
      kind: k3d
      context: k3d-dev
  - name: staging
    namespace: product
    cluster:
      kind: gke
      context: staging
    key-service: kms
    managed-services:
      store:
        kind: cloud-sql-postgres
        external-name: 127.0.0.1
        port: 5432
        auth-mode: external-identity
        instance-connection-name: project-1:us-east1:store
      cache:
        kind: redis
        external-name: cache.data.svc.cluster.local
        port: 6379
        egress-namespace: data
`

func loadCarriedWorkspace(t *testing.T, document string) *resources.Workspace {
	t.Helper()
	workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte(document))
	require.NoError(t, err)
	return workspace
}

// Each module-owned key loads, is held verbatim, and survives the round trip
// back to the workspace document.
func TestCarriedKeysLoadAndRoundTrip(t *testing.T) {
	workspace := loadCarriedWorkspace(t, carriedWorkspace)
	staging, err := environments.Select(workspace, "staging")
	require.NoError(t, err)

	require.Equal(t, "kms", staging.Carried["key-service"].Value)
	store := staging.ManagedServices["store"]
	require.Equal(t, "external-identity", store.Carried["auth-mode"].Value)
	require.Equal(t, "project-1:us-east1:store", store.Carried["instance-connection-name"].Value)
	require.Equal(t, "data", staging.ManagedServices["cache"].Carried["egress-namespace"].Value)
	// The CLI's own fields are unaffected by the keys beside them.
	require.Equal(t, 5432, store.Port)

	resource, err := staging.Resource()
	require.NoError(t, err)
	again, err := environments.FromRuntime(resource)
	require.NoError(t, err)
	// Compared as documents: a carried value keeps its source position, which
	// a re-encoded map does not reproduce.
	before, err := yaml.Marshal(staging)
	require.NoError(t, err)
	after, err := yaml.Marshal(again)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	require.Contains(t, string(after), "key-service: kms")
}

// A carried key is the module agent's, not a service agent's: nothing of it
// reaches the runtime environment agents are handed.
func TestCarriedKeysNeverReachTheRuntime(t *testing.T) {
	staging, err := environments.Select(loadCarriedWorkspace(t, carriedWorkspace), "staging")
	require.NoError(t, err)
	require.Empty(t, staging.Runtime().Extensions)
	data, err := yaml.Marshal(staging.Runtime())
	require.NoError(t, err)
	require.NotContains(t, string(data), "key-service")
}

// A key declared on one deployed environment does not break the workspace
// load, nor the local environment a `codefly run` selects.
func TestCarriedKeyOnADeployedEnvironmentLeavesTheLocalRunLoading(t *testing.T) {
	workspace := loadCarriedWorkspace(t, carriedWorkspace)
	all, err := environments.FromWorkspace(workspace)
	require.NoError(t, err)
	require.Len(t, all, 2)
	local, err := environments.Select(workspace, "local")
	require.NoError(t, err)
	require.Empty(t, local.Carried)
	require.True(t, local.Local())
}

// The seam is closed: a key that is neither a CLI field nor a carried one is
// refused by name, wherever it is written, so a typo still fails the load.
func TestCarriedSeamStillRefusesAnUnknownKeyByName(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{"environment", "key-service: kms", "key-servce: kms", `field "key-servce" not found in environment`},
		{"managed service", "auth-mode: external-identity", "auth-mod: external-identity", `field "auth-mod" not found in managed service "store"`},
		{"carried key on the wrong block", "egress-namespace: data", "key-service: kms", `field "key-service" not found in managed service "cache"`},
		{"managed key on the environment", "    key-service: kms", "    auth-mode: password", `field "auth-mode" not found in environment`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := strings.Replace(carriedWorkspace, tc.from, tc.to, 1)
			require.NotEqual(t, carriedWorkspace, document)
			_, err := environments.FromWorkspace(loadCarriedWorkspace(t, document))
			require.ErrorContains(t, err, tc.want)
			// The refusal names the environment it was written in.
			require.ErrorContains(t, err, `environment "staging"`)
		})
	}
	// A nested CLI block keeps the strict decoder: the seam is the
	// environment's and the managed service's own keys, nothing below them.
	nested := strings.Replace(carriedWorkspace, "      context: staging", "      contxt: staging", 1)
	_, err := environments.FromWorkspace(loadCarriedWorkspace(t, nested))
	require.ErrorContains(t, err, "contxt")
}

// A producer's coordinate contract may carry the keys too, and a typo there is
// refused the same way.
func TestCoordinateContractCarriesModuleOwnedKeys(t *testing.T) {
	contract, err := environments.ParseCoordinateContract([]byte(`{"schema":"codefly/coordinate/v1","environment":{"name":"staging","namespace":"product","key-service":"kms"}}`))
	require.NoError(t, err)
	require.Equal(t, "kms", contract.Environment.Carried["key-service"].Value)
	_, err = environments.ParseCoordinateContract([]byte(`{"schema":"codefly/coordinate/v1","environment":{"name":"staging","namespace":"product","key_service":"kms"}}`))
	require.ErrorContains(t, err, `field "key_service" not found in environment`)
	_, err = environments.ParseCoordinateContract([]byte(`{"schema":"codefly/coordinate/v1","environment":{"name":"staging","namespace":"product","managed-services":{"store":{"kind":"postgres","external-name":"db.example","port":5432,"auth-mod":"password"}}}}`))
	require.ErrorContains(t, err, `field "auth-mod" not found in managed service "store"`)
}

// A JSON projection of an environment writes a carried value as the value it
// spells, not the YAML node holding it.
func TestCarriedKeysMarshalAsPlainJSON(t *testing.T) {
	staging, err := environments.Select(loadCarriedWorkspace(t, carriedWorkspace), "staging")
	require.NoError(t, err)
	data, err := json.Marshal(staging.Carried)
	require.NoError(t, err)
	require.JSONEq(t, `{"key-service":"kms"}`, string(data))
	data, err = json.Marshal(staging.ManagedServices["store"].Carried)
	require.NoError(t, err)
	require.JSONEq(t, `{"auth-mode":"external-identity","instance-connection-name":"project-1:us-east1:store"}`, string(data))
}
