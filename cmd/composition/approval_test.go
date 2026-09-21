package composition

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	selection "github.com/codefly-dev/cli/pkg/composition"
	core "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/policy"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestApprovalSigningKeyIsPrivateAndErrorsDoNotExposeBytes(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, private, 0o600))
	key, err := readApprovalKey(path)
	require.NoError(t, err)
	require.Equal(t, private, key)
	require.NoError(t, os.Chmod(path, 0o644))
	_, err = readApprovalKey(path)
	require.ErrorContains(t, err, "private regular file")
	require.NotContains(t, err.Error(), string(private))
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))
	_, err = readApprovalKey(path)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}

func TestApprovalCommandsRequireExplicitChoicesAndHaveOfflineHelp(t *testing.T) {
	cmd := NewCommand()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"approve-admission", "inputs", "record", "/tmp/approval"})
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "required flag(s)")
	for _, name := range []string{"configure-approval-authority", "inspect-approval-authority", "approve-admission", "check-approval", "inspect-approval-use"} {
		cmd = NewCommand()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{name, "--help"})
		require.NoError(t, cmd.ExecuteContext(t.Context()))
		require.Contains(t, output.String(), name)
	}
	cmd = NewCommand()
	approve, _, err := cmd.Find([]string{"approve-admission"})
	require.NoError(t, err)
	require.Nil(t, approve.Flags().Lookup("policy"), "candidate requests cannot choose verification policy")
	require.Nil(t, approve.Flags().Lookup("public-key"))
}

func TestApprovalAuthorityCommandInstallsAndExplicitlyRotatesHostPolicy(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, home)
	t.Setenv("GITHUB_TOKEN", "test-local-configuration-no-network")
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte("module-trust: {}\n"), 0o600))
	configuration, identityKey := filepath.Join(root, "configuration.json"), filepath.Join(root, "identity-key")
	require.NoError(t, os.WriteFile(configuration, []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(identityKey, bytes.Repeat([]byte{1}, 32), 0o600))
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	config := selection.ApprovalAuthorityConfig{Audience: "reviewed-product", Approver: policy.Principal{ID: "operator", Kind: policy.KindHuman}, Key: public,
		Policy:   core.DeploymentPolicy{RequiredQualifications: []string{"functional"}, QualificationSigners: map[string]map[string]ed25519.PublicKey{"functional": {"qualifier": public}}},
		Bindings: map[string]string{"target": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "authority.json")
	require.NoError(t, os.WriteFile(configPath, data, 0o600))
	run := func(args ...string) (selection.ApprovalAuthorityInspection, error) {
		t.Helper()
		cmd := NewCommand()
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(append(args, "--workspace", root, "--product", root, "--configuration", configuration, "--identity-key", identityKey))
		var result selection.ApprovalAuthorityInspection
		if commandErr := cmd.ExecuteContext(t.Context()); commandErr != nil {
			return result, commandErr
		}
		require.NoError(t, json.Unmarshal(output.Bytes(), &result))
		return result, nil
	}
	installed, err := run("configure-approval-authority", configPath)
	require.NoError(t, err)
	require.NotEmpty(t, installed.Digest)
	inspected, err := run("inspect-approval-authority")
	require.NoError(t, err)
	require.Equal(t, installed, inspected)
	_, err = run("configure-approval-authority", configPath)
	require.ErrorContains(t, err, "explicit current digest")
	config.Audience = "explicitly-changed-audience"
	data, err = json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, data, 0o600))
	replaced, err := run("configure-approval-authority", configPath, "--expected-digest", installed.Digest)
	require.NoError(t, err)
	require.NotEqual(t, installed.Digest, replaced.Digest)
	_, err = run("configure-approval-authority", configPath, "--expected-digest", installed.Digest)
	require.ErrorContains(t, err, "explicit current digest")
}
