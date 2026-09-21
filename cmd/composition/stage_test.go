package composition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStagingRequiresExplicitPrincipalAndSandboxChoices(t *testing.T) {
	for _, flags := range []stageFlags{
		{},
		{outputParent: "/tmp", sandbox: "required"},
		{outputParent: "/tmp", sandbox: "unknown", withoutPrincipal: true},
		{outputParent: "/tmp", sandbox: "none", withoutPrincipal: true, allowNetwork: true},
	} {
		_, err := flags.loadOptions("/tmp")
		require.Error(t, err)
	}
	flags := stageFlags{outputParent: "/tmp", sandbox: "none", withoutPrincipal: true}
	_, err := flags.loadOptions("/tmp")
	require.NoError(t, err)
}

func TestRenderConfigurationRejectsMissingAndMalformedInputsWithoutSecrets(t *testing.T) {
	_, _, err := readRenderConfiguration("", "key")
	require.ErrorContains(t, err, "--render-requests")
	path := filepath.Join(t.TempDir(), "render.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"unexpected-secret":true}]`), 0o600))
	_, _, err = readRenderConfiguration(path, "key")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "unexpected-secret")
}

func TestRenderAndConfigurationModesAreMutuallyExclusive(t *testing.T) {
	command := NewCommand()
	command.SilenceErrors, command.SilenceUsage = true, true
	command.SetArgs([]string{"inspect", "--configuration", "config", "--render-requests", "requests"})
	require.ErrorContains(t, command.ExecuteContext(t.Context()), "were all set")
}
