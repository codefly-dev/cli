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
