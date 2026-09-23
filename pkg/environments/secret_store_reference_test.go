package environments

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestRemoteSecretStoreImportRoundTrip(t *testing.T) {
	input := `schema: codefly/coordinate/v1
environment:
  name: staging
  namespace: product
  service-secrets:
    secret-store: {name: cell-secrets, kind: ClusterSecretStore}
    defaults: {key: "{module}-{service}", property: "{key}"}
    services:
      accounts:
        remote-keys:
          CLIENT_SECRET:
            key: product-identity
            property: client_secret
            secret-store: {name: shared-identity, kind: ClusterSecretStore}
          TOKEN:
            key: shared-token
            secret-store: {name: shared-identity, kind: ClusterSecretStore}
`
	jsonInput := func(input string) []byte {
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(input), &doc))
		data, err := json.Marshal(doc)
		require.NoError(t, err)
		return data
	}
	contract, err := ParseCoordinateContract(jsonInput(input))
	require.NoError(t, err)
	env, err := contract.ToEnvironment("staging", "product")
	require.NoError(t, err)
	encoded, err := yaml.Marshal(env)
	require.NoError(t, err)
	var roundTrip Environment
	require.NoError(t, yaml.Unmarshal(encoded, &roundTrip))
	require.Equal(t, *env, roundTrip)
	scope := SecretScope{Module: "saas", Service: "accounts"}
	shared := env.ServiceSecrets.RemoteRef(scope, "CLIENT_SECRET")
	require.Equal(t, &EnvironmentSecretStoreReference{Name: "shared-identity", Kind: "ClusterSecretStore"}, shared.SecretStore)
	require.Nil(t, env.ServiceSecrets.RemoteRef(scope, "PASSWORD").SecretStore)
	shared.SecretStore.Name = "changed"
	require.Equal(t, "shared-identity", contract.Environment.ServiceSecrets.RemoteRef(scope, "CLIENT_SECRET").SecretStore.Name)

	for _, replacement := range []string{
		"secret-store: {name: shared-identity, kind: Vault}",
		"secret-store: {name: '', kind: ClusterSecretStore}",
		"secret-store: {name: shared-identity, kind: ClusterSecretStore, namesapce: other}",
	} {
		_, err := ParseCoordinateContract(jsonInput(strings.ReplaceAll(input, "secret-store: {name: shared-identity, kind: ClusterSecretStore}", replacement)))
		require.Error(t, err, replacement)
	}
}

func TestSecretDefaultsPreserveSelectedStore(t *testing.T) {
	shared := &EnvironmentSecretStoreReference{Name: "shared-identity", Kind: "ClusterSecretStore"}
	secrets := &EnvironmentServiceSecrets{
		SecretStore: EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
		Defaults:    &EnvironmentSecretRemoteRef{Key: "{module}-{service}", Property: "{key}", SecretStore: shared},
		Services: map[string]EnvironmentServiceSecretMapping{
			"accounts": {Defaults: &EnvironmentSecretRemoteRef{Key: "accounts", Property: "{key}"}},
		},
	}
	require.NoError(t, secrets.Validate())
	require.Equal(t, EnvironmentSecretRemoteRef{Key: "saas-api", Property: "TOKEN", SecretStore: shared}, secrets.RemoteRef(SecretScope{Module: "saas", Service: "api"}, "TOKEN"))
	require.Nil(t, secrets.RemoteRef(SecretScope{Module: "saas", Service: "accounts"}, "TOKEN").SecretStore)
	shared.Kind = "Invalid"
	require.ErrorContains(t, secrets.Validate(), "defaults secret-store")
}
