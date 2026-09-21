package composition

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectionCommandRequiresExplicitConfigurationWithoutLeakingSecrets(t *testing.T) {
	cmd := NewCommand()
	cmd.SetArgs([]string{"inspect"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "--configuration and --identity-key are required")
	require.Empty(t, output.String())
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	key := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(config, []byte(`{"password":"secret"}`), 0o600))
	require.NoError(t, os.WriteFile(key, []byte("too short"), 0o600))
	cmd = NewCommand()
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"inspect", "--configuration", config, "--identity-key", key})
	err := cmd.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "256-bit key")
	require.NotContains(t, err.Error(), "secret")
	require.Empty(t, output.String())
}

func TestSelectionInputRejectsUnknownFieldsAndTrailingDocuments(t *testing.T) {
	for _, data := range []string{`{"unexpected":true}`, `{"name":"one"} {"name":"two"}`} {
		path := filepath.Join(t.TempDir(), "input.json")
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
		var target struct {
			Name string `json:"name"`
		}
		require.Error(t, readJSON(path, &target))
	}
}

func TestPrepareRenderRequiresExplicitConfiguration(t *testing.T) {
	cmd := NewCommand()
	cmd.SetArgs([]string{"prepare-render", "inputs.json"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "--configuration and --identity-key are required")
	require.Empty(t, output.String())
}

func TestAdmissionRecordCommandsRequireExplicitInputs(t *testing.T) {
	for _, args := range [][]string{
		{"record-admission", "inputs.json", "policy.json", "/tmp/admission.json"},
		{"recheck-admission", "inputs.json", "policy.json", "record.json", "--expected-identity", "retained"},
	} {
		cmd := NewCommand()
		cmd.SetArgs(args)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "--configuration and --identity-key are required")
	}
	cmd := NewCommand()
	cmd.SetArgs([]string{"recheck-admission", "inputs.json", "policy.json", "record.json"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), `required flag(s) "expected-identity" not set`)
}
