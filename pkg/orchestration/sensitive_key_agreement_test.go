package orchestration

import (
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The CLI owns no sensitive-key list: restricted rendering here and core's
// restricted-render guard both classify through resources.IsSensitiveKey, so the
// marker list — and any narrowing core makes to it — has one owner. This test
// pins the agreement rather than the list: whatever core classifies as
// sensitive, the CLI must either promote to a secretKeyRef or refuse as a
// misplacement, and must never forward inline; whatever core does not, the CLI
// must not refuse. A change in core that the CLI does not mirror fails here.
func TestRestrictedRenderClassificationAgreesWithCore(t *testing.T) {
	keys := []string{
		// Federation carriers a composed solution's render references from the
		// secret store — each must stay credential-classified.
		"CODEFLY__MODULE_REGISTRATION_SECRETS",
		"CODEFLY__MODULE_IDENTITY_SECRET",
		"CODEFLY__MODULE_REGISTRATION_SECRET",
		"CODEFLY__SOLUTION_REGISTRATION_SECRET",
		"MODULE_REGISTRATION_SECRETS",
		"SOLUTION_REGISTRATION_SECRETS",
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
		sensitive := resources.IsSensitiveKey(key)

		require.Equal(t, sensitive, restrictedRenderRejects(value),
			"%s: the CLI's mirror of core's restricted-render guard disagrees with core's classification", key)

		if isSecretEndpointKey(key) {
			require.True(t, sensitive, "%s: the CLI narrows a key core does not classify as sensitive", key)
		}

		misplaced := misplacedSecretKeys([]*basev0.Configuration{{
			Origin: "service",
			Infos:  []*basev0.ConfigurationInformation{{Name: "example", ConfigurationValues: []*basev0.ConfigurationValue{value}}},
		}})
		if sensitive {
			require.True(t, promotesToDeploymentSecret(value) || len(misplaced) == 1,
				"%s: core classifies it as sensitive, yet the CLI would forward its plaintext inline", key)
		} else {
			require.False(t, promotesToDeploymentSecret(value), "%s: promoted although core does not classify it", key)
			require.Empty(t, misplaced, "%s: refused although core does not classify it", key)
		}
	}

	for _, key := range []string{"CODEFLY__MODULE_REGISTRATION_SECRETS", "CODEFLY__MODULE_IDENTITY_SECRET", "CODEFLY__MODULE_REGISTRATION_SECRET"} {
		require.True(t, resources.IsSensitiveKey(key), "%s must be credential-classified", key)
	}
	for _, key := range []string{"CODEFLY__API_CONSUMES", "CODEFLY__MODULE_IDENTITY_PREFIX"} {
		require.False(t, resources.IsSensitiveKey(key), "%s is rendered as a value and must not be credential-classified", key)
	}
}
