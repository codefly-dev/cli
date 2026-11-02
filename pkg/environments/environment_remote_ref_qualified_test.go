package environments

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Eleven modules of one composition each ship a service called "store", so a
// bare name cannot say which one an environment means. Before this, the only
// expressible form was the bare one and the stores could not be named at all:
// platform-obin's blue render refused with `service-secrets references unknown
// service "tracing/store"`, and without an explicit reference every store's
// POSTGRES_USER fell through to the dev plaintext its module ships, which
// service-postgres refuses.
func TestAModuleQualifiedSecretsEntryNamesOneModulesService(t *testing.T) {
	secrets := &EnvironmentServiceSecrets{
		Defaults: &EnvironmentSecretRemoteRef{Key: "cell-{module}-{service}", Property: "{key}"},
		Services: map[string]EnvironmentServiceSecretMapping{
			"model/store": {RemoteKeys: map[string]EnvironmentSecretRemoteRef{
				"POSTGRES_USER": {Key: "cell-model-store", Property: "POSTGRES_USER"},
			}},
			"tracing/store": {RemoteKeys: map[string]EnvironmentSecretRemoteRef{
				"POSTGRES_USER": {Key: "cell-tracing-store", Property: "POSTGRES_USER"},
			}},
		},
	}

	model := secrets.RemoteRef(SecretScope{Workspace: "platform-obin", Module: "model", Service: "store"}, "POSTGRES_USER")
	require.Equal(t, "cell-model-store", model.Key)
	tracing := secrets.RemoteRef(SecretScope{Workspace: "platform-obin", Module: "tracing", Service: "store"}, "POSTGRES_USER")
	require.Equal(t, "cell-tracing-store", tracing.Key)

	// A module with no entry of its own still falls to the environment template,
	// which the scope's module keeps distinct.
	other := secrets.RemoteRef(SecretScope{Workspace: "platform-obin", Module: "records", Service: "store"}, "POSTGRES_USER")
	require.Equal(t, "cell-records-store", other.Key)
	require.Equal(t, "POSTGRES_USER", other.Property)
}

// A bare entry keeps working for a name only one module declares: every
// environment written before the qualified form existed states its services
// that way.
func TestABareSecretsEntryStillNamesItsService(t *testing.T) {
	secrets := &EnvironmentServiceSecrets{
		Services: map[string]EnvironmentServiceSecretMapping{
			"accounts": {RemoteKeys: map[string]EnvironmentSecretRemoteRef{
				"TOKEN": {Key: "cell-saas-accounts", Property: "TOKEN"},
			}},
		},
	}
	ref := secrets.RemoteRef(SecretScope{Workspace: "platform-obin", Module: "saas", Service: "accounts"}, "TOKEN")
	require.Equal(t, "cell-saas-accounts", ref.Key)
}
