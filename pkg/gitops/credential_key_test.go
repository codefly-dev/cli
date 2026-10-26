package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The credential scan of a configuration carrier reads the configuration key
// core's parser isolates, never the structural segments in front of it: a
// group named for a policy about credentials does not make its CLIENT_ID a
// credential, while a key that names one, or that core calls sensitive, still
// is. A name that is not a carrier is scanned whole, as every schema field is.
func TestTheCredentialScanReadsACarriersKeyAlone(t *testing.T) {
	for name, want := range map[string]bool{
		// The review's three policy cases: public keys under credential-sounding groups.
		"CODEFLY__WORKSPACE_CONFIGURATION__CREDENTIAL_POLICY__CLIENT_ID":    false,
		"CODEFLY__WORKSPACE_CONFIGURATION__PASSWORD_POLICY__CLIENT_ID":      false,
		"CODEFLY__WORKSPACE_CONFIGURATION__CLIENT_SECRET_POLICY__CLIENT_ID": false,
		// The same groups, with a key that names a credential.
		"CODEFLY__WORKSPACE_CONFIGURATION__CREDENTIAL_POLICY__PASSWORD":         true,
		"CODEFLY__WORKSPACE_CONFIGURATION__PASSWORD_POLICY__ADMIN_CREDENTIAL":   true,
		"CODEFLY__WORKSPACE_CONFIGURATION__CLIENT_SECRET_POLICY__CLIENT_SECRET": true,
		// A service carrier has three structural segments before its key.
		"CODEFLY__SERVICE_CONFIGURATION__SAAS__AUTH_GATEWAY__CREDENTIAL_POLICY__CLIENT_ID": false,
		"CODEFLY__SERVICE_CONFIGURATION__SAAS__AUTH_GATEWAY__GATEWAY__PRIVATE_KEY":         true,
		// What core calls sensitive stays sensitive, whatever the scan says.
		"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__DATASOURCE_OAUTH__CLIENT_SECRET": true,
		"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__DATASOURCE_OAUTH__ACCESS_TOKEN":  true,
		"CODEFLY__WORKSPACE_CONFIGURATION__DATASOURCE_OAUTH__CLIENT_ID":            false,
		"CODEFLY__WORKSPACE_CONFIGURATION__DATASOURCE_OAUTH__PROVIDERS":            false,
		// An endpoint carrier is an address, whatever the service is called.
		"CODEFLY__ENDPOINT__SAAS__AUTH_GATEWAY__GRPC__GRPC": false,
		// A name that is no carrier is scanned whole.
		"DATABASE_PASSWORD": true,
		"MAX_OUTPUT_TOKENS": false,
	} {
		require.Equal(t, want, isCredentialKey(name, true), name)
	}
	// Schema fields keep the camelCase scan over the whole field.
	require.True(t, isCredentialKey("privateKey", false))
	require.True(t, isCredentialKey("clientSecretRef", false))
	require.False(t, isCredentialKey("sessionAffinity", false))
	require.False(t, isCredentialKey("secretName", false))
}

// The guard over a rendered ConfigMap applies the same reading: a public
// configuration key under a credential-sounding group is ordinary
// configuration the render keeps, and a key that names a credential under the
// same group is a leaked value the render refuses.
func TestRenderKeepsAPublicKeyUnderACredentialSoundingGroup(t *testing.T) {
	render := func(data string) error {
		_, err := RenderOwnedTree(context.Background(), &RenderOptions{
			Destination: filepath.Join(t.TempDir(), "owned"),
			Module:      "payments", Environment: "production", Promotable: true,
		}, func(ctx context.Context, root string) error {
			manifest := pinnedDeployment + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: policies\ndata:\n" + data
			return os.WriteFile(filepath.Join(root, "manifests.yaml"), []byte(manifest), 0o644)
		})
		return err
	}
	require.NoError(t, render(
		"  CODEFLY__WORKSPACE_CONFIGURATION__CREDENTIAL_POLICY__CLIENT_ID: policies-client\n"+
			"  CODEFLY__WORKSPACE_CONFIGURATION__PASSWORD_POLICY__CLIENT_ID: policies-client\n"+
			"  CODEFLY__WORKSPACE_CONFIGURATION__CLIENT_SECRET_POLICY__CLIENT_ID: policies-client\n"+
			"  CODEFLY__SERVICE_CONFIGURATION__SAAS__AUTH_GATEWAY__CREDENTIAL_POLICY__CLIENT_ID: policies-client\n"))
	err := render("  CODEFLY__WORKSPACE_CONFIGURATION__CREDENTIAL_POLICY__PASSWORD: plaintext\n")
	require.ErrorContains(t, err, "credential value")
	err = render("  CODEFLY__WORKSPACE_SECRET_CONFIGURATION__CREDENTIAL_POLICY__CLIENT_ID: plaintext\n")
	require.ErrorContains(t, err, "credential value", "a secret carrier is sensitive to core whatever its key")
}

// A public OAuth client identifier is not a credential.
//
// A client id identifies the client to an authorization server and is published
// in redirects, in discovery documents and in browser requests; the matching
// secret is the client SECRET. Core already calls a bare CLIENT_ID public. What
// it still called sensitive is the spelling an operator writes —
// OAUTH_CLIENT_ID, AUTH_CLIENT_ID — and only through the AUTH word in front of
// it, so the render refused a public value and the only way to deliver it was a
// secretKeyRef into a store entry somebody had to fill by hand.
//
// Both sides are pinned, because the narrowing is a name heuristic and the cost
// of getting it wrong is asymmetric: a public value refused costs an operator a
// hand write, a credential accepted commits it to a repository.
func TestTheRenderGuardAcceptsAPublicClientIdAndRefusesACredential(t *testing.T) {
	for name, want := range map[string]bool{
		// Public: the client identifier, in every spelling of it.
		"client_id":        false,
		"client-id":        false,
		"clientId":         false,
		"CLIENT_ID":        false,
		"oauth_client_id":  false,
		"OAUTH_CLIENT_ID":  false,
		"AUTH_CLIENT_ID":   false,
		"OAUTH2_CLIENT_ID": false,
		"CODEFLY__WORKSPACE_CONFIGURATION__IDENTITY__OAUTH_CLIENT_ID":                   false,
		"CODEFLY__SERVICE_CONFIGURATION__SAAS__AUTH_GATEWAY__IDENTITY__OAUTH_CLIENT_ID": false,
		// Credentials, under the same AUTH words: the secret, the token, the
		// password and the key keep their refusal.
		"client_secret":       true,
		"CLIENT_SECRET":       true,
		"OAUTH_CLIENT_SECRET": true,
		"OAUTH_ACCESS_TOKEN":  true,
		"AUTH_PASSWORD":       true,
		"OAUTH_CLIENT_KEY":    true,
		// A name ending in the client-identifier spelling whose credential
		// signal is NOT the AUTH word: the marker survives the strip, so core
		// still classifies it and nothing is narrowed.
		"OAUTH_TOKEN_CLIENT_ID": true,
		"CLIENT_ID_SECRET":      true,
		// A SECRET carrier is sensitive whatever its key: the value was
		// authored as a secret, and that declaration outranks its name.
		"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__IDENTITY__OAUTH_CLIENT_ID": true,
	} {
		require.Equal(t, want, isCredentialKey(name, true), name)
	}
}

// End to end: the render that refused a public client id, and the one that must
// keep refusing a client secret beside it.
func TestRenderAcceptsAPlainClientIdAndRefusesAPlainClientSecret(t *testing.T) {
	render := func(data string) error {
		_, err := RenderOwnedTree(context.Background(), &RenderOptions{
			Destination: filepath.Join(t.TempDir(), "owned"),
			Module:      "payments", Environment: "production", Promotable: true,
		}, func(ctx context.Context, root string) error {
			manifest := pinnedDeployment + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: identity\ndata:\n" + data
			return os.WriteFile(filepath.Join(root, "manifests.yaml"), []byte(manifest), 0o644)
		})
		return err
	}
	require.NoError(t, render(
		"  OAUTH_CLIENT_ID: public-client-identifier\n"+
			"  AUTH_CLIENT_ID: public-client-identifier\n"+
			"  CODEFLY__WORKSPACE_CONFIGURATION__IDENTITY__OAUTH_CLIENT_ID: public-client-identifier\n"))
	for _, data := range []string{
		"  OAUTH_CLIENT_SECRET: plaintext\n",
		"  OAUTH_ACCESS_TOKEN: plaintext\n",
		"  AUTH_PASSWORD: plaintext\n",
		"  OAUTH_CLIENT_KEY: plaintext\n",
	} {
		require.ErrorContains(t, render(data), "credential value", data)
	}
}
