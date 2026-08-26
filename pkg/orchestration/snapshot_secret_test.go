package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// staticConfigurationLoader feeds a declared configuration into a real
// configurations.Manager so the secret-resolution path can be exercised without
// a live provider. It mirrors the declared reference-only manifests a workspace
// commits: keys and provider references, never plaintext values.
type staticConfigurationLoader struct {
	confs []*basev0.Configuration
}

func (staticConfigurationLoader) Identity() string { return "static-test-loader" }

func (staticConfigurationLoader) Load(context.Context, *resources.Environment) error { return nil }

func (l staticConfigurationLoader) Configurations() []*basev0.Configuration { return l.confs }

func (staticConfigurationLoader) DNS() []*basev0.DNS { return nil }

func declaredSecretConfiguration() *basev0.Configuration {
	return &basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "auth",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "client_secret", Value: "op://dev-vault/auth/client_secret", Secret: true},
			},
		}},
	}
}

func TestSnapshotSecretResolverAnswersReferencesWithoutAProvider(t *testing.T) {
	ref, ok := configurations.ParseSecretReference("op://dev-vault/auth/client_secret")
	require.True(t, ok)

	resolver := snapshotSecretResolver{}
	require.Equal(t, configurations.OnePasswordScheme, resolver.Scheme())

	value, err := resolver.Resolve(context.Background(), ref)
	require.NoError(t, err)
	require.NotEmpty(t, value)
}

// Without a resolver and with no provider configured, resolving a reference-only
// secret fails — this is the "render demands local value files" pressure the
// snapshot resolver removes.
func TestManagerRejectsReferenceOnlySecretWithoutAResolver(t *testing.T) {
	ctx := context.Background()
	manager, err := configurations.NewManager(ctx, &resources.Workspace{})
	require.NoError(t, err)
	manager.WithLoader(staticConfigurationLoader{confs: []*basev0.Configuration{declaredSecretConfiguration()}})

	err = manager.Load(ctx, &resources.Environment{Name: "staging"})
	require.Error(t, err)
}

// With the snapshot resolver registered, the same reference-only declaration
// loads without a provider and the value is replaced by the inert placeholder,
// which promotableConfiguration then discards into a secretKeyRef.
func TestSnapshotSecretResolverMakesReferenceOnlyRenderValueFree(t *testing.T) {
	ctx := context.Background()
	loader := staticConfigurationLoader{confs: []*basev0.Configuration{declaredSecretConfiguration()}}
	manager, err := configurations.NewManager(ctx, &resources.Workspace{})
	require.NoError(t, err)
	manager.WithLoader(loader)
	manager.WithSecretResolver(snapshotSecretResolver{})

	require.NoError(t, manager.Load(ctx, &resources.Environment{Name: "staging"}))

	resolved := loader.Configurations()[0]
	require.Equal(t, snapshotSecretPlaceholder, resolved.GetInfos()[0].GetConfigurationValues()[0].GetValue())

	sanitized, _, references, err := promotableDeploymentConfigurations(resolved, nil, "secret-accounts")
	require.NoError(t, err)
	require.Empty(t, sanitized.GetInfos())
	require.Len(t, references, 1)
	for _, reference := range references {
		require.Equal(t, "secret-accounts", reference.GetName())
	}
}
