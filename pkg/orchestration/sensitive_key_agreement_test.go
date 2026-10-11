package orchestration

import (
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The CLI owns no sensitive-key list: promotion and the GitOps render guard
// classify carriers through resources.IsSensitiveEnvironmentVariable, so the
// marker list — and any narrowing core makes to it — has one owner. This test
// pins the agreement rather than the list: whatever core classifies as
// sensitive, the CLI must either promote to a secretKeyRef or refuse as a
// misplacement, and must never forward inline; whatever core does not, the CLI
// must not refuse. A change in core that the CLI does not mirror fails here.
func TestRestrictedRenderClassificationAgreesWithCore(t *testing.T) {
	keys := []string{
		// A module's identity secret, which a render references from the
		// secret store — it must stay credential-classified.
		"CODEFLY__MODULE_IDENTITY_SECRET",
		// Federation carriers a render writes as values — each must stay public.
		"CODEFLY__API_CONSUMES",
		"CODEFLY__MODULE_IDENTITY_PREFIX",
		// Identity endpoint keys the CLI narrows out of promotion.
		"OIDC_AUTHORIZE_URL",
		"OIDC_TOKEN_URL",
		"AUTH_SELECTOR",
		"CODEFLY__SELF_ENDPOINT__HOST__AUTH_GATEWAY__REST__REST",
		// Conventional credentials and plain configuration.
		"DATABASE_URL", "PASSWORD", "API_KEY", "SESSION_COOKIE", "PRIVATE_KEY",
		"ACCESS_TOKEN_URL", "CLIENT_SECRET_URL", "LOG_LEVEL", "GATEWAY_PATH", "REGION",
	}
	for _, key := range keys {
		value := &basev0.ConfigurationValue{Key: key, Value: "value"}
		configuration := &basev0.Configuration{
			Origin: "example/api",
			Infos:  []*basev0.ConfigurationInformation{{Name: "settings", ConfigurationValues: []*basev0.ConfigurationValue{value}}},
		}
		sensitive := resources.IsSensitiveEnvironmentVariable(resources.ConfigurationValueEnvironmentKey(configuration, "settings", value))

		require.Equal(t, sensitive, restrictedRenderRejects(configuration, "settings", value),
			"%s: the CLI's mirror of core's restricted-render guard disagrees with core's classification", key)

		if isSecretEndpointKey(key) {
			require.True(t, sensitive, "%s: the CLI narrows a key core does not classify as sensitive", key)
		}

		misplaced := misplacedSecretKeys([]*basev0.Configuration{configuration})
		if sensitive {
			require.True(t, promotesToDeploymentSecret(configuration, "settings", value) || len(misplaced) == 1,
				"%s: core classifies it as sensitive, yet the CLI would forward its plaintext inline", key)
		} else {
			require.False(t, promotesToDeploymentSecret(configuration, "settings", value), "%s: promoted although core does not classify it", key)
			require.Empty(t, misplaced, "%s: refused although core does not classify it", key)
		}
	}

	for _, key := range []string{"CODEFLY__MODULE_IDENTITY_SECRET"} {
		require.True(t, resources.IsSensitiveKey(key), "%s must be credential-classified", key)
	}
	for _, key := range []string{"CODEFLY__API_CONSUMES", "CODEFLY__MODULE_IDENTITY_PREFIX"} {
		require.False(t, resources.IsSensitiveKey(key), "%s is rendered as a value and must not be credential-classified", key)
	}
}

// A group belongs to the configuration name the guard sees; the service's
// identity does not. Comparing the bare key or scanning the whole carrier as a
// key loses one of those boundaries.
func TestConfigurationCarrierClassification(t *testing.T) {
	for _, test := range []struct {
		name, origin, group, key string
		sensitive                bool
	}{
		{"workspace group", resources.ConfigurationWorkspace, "datasource-oauth", "providers", true},
		{"service group", "saas/auth-gateway", "datasource-oauth", "providers", true},
		{"service identity", "saas/auth-gateway", "identity", "audience", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := &basev0.Configuration{Origin: test.origin}
			value := &basev0.ConfigurationValue{Key: test.key, Value: "{}"}
			require.Equal(t, test.sensitive, restrictedRenderRejects(configuration, test.group, value))
			require.Equal(t, test.sensitive, isCredentialValue(configuration, test.group, value))
			require.Equal(t, test.sensitive, promotesToDeploymentSecret(configuration, test.group, value))
		})
	}
}

func TestCredentialsOfAgreesWithGroupSensitivePromotion(t *testing.T) {
	configuration := workspaceConfiguration("datasource-oauth", "providers", "{}")
	withheld := credentialsOf(configuration.GetInfos(), map[string]bool{"datasource-oauth": true})
	require.Equal(t, map[string]bool{"datasource-oauth/providers": true}, withheld.values)
	received := withheld.received(configuration.GetInfos())
	require.Len(t, received, 1)
	require.Empty(t, received[0].GetConfigurationValues())

	_, safe, references, err := promotableDeploymentConfigurations(nil, []*basev0.Configuration{configuration}, "secret-api")
	require.NoError(t, err)
	require.Empty(t, safe[0].GetInfos())
	key := "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__DATASOURCE_OAUTH__PROVIDERS"
	require.Len(t, references, 1)
	require.Equal(t, "secret-api", references[key].GetName())
	require.Equal(t, key, references[key].GetKey())

	// A service that declares the group receives it; the caller supplies only
	// the groups it did not declare to the withholding decision.
	require.False(t, credentialsOf(configuration.GetInfos(), map[string]bool{"other": true}).any())
}
