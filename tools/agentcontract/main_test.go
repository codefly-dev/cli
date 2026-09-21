package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestReleaseCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous *agentv0.AgentContract
		want     string
	}{
		{"introduction", nil, "agents without a protocol declaration must adopt"},
		{"unchanged", contract.Current(), "No agent rebuild"},
		{"lifecycle changed", &agentv0.AgentContract{ProtocolVersion: 2, StartupProtocolVersion: 2}, "Agent protocol changed"},
		{"startup changed", &agentv0.AgentContract{ProtocolVersion: 1, StartupProtocolVersion: 1}, "Agent protocol changed"},
		{"capabilities changed", &agentv0.AgentContract{ProtocolVersion: 1, StartupProtocolVersion: 2}, "Required capabilities changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notes := releaseNotes(contract.Current(), tc.previous, "v0.1.160")
			require.Contains(t, notes, tc.want)
			require.Contains(t, notes, contract.ContainerRecoveryScope)
			require.Contains(t, notes, "Native/Nix")
			if tc.name != "unchanged" {
				require.NotContains(t, notes, "No agent rebuild")
			}
		})
	}
}

func TestPreviousStableTag(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-q")
	git("config", "user.name", "Contract test")
	git("config", "user.email", "contract@example.invalid")
	git("config", "commit.gpgSign", "false")
	git("config", "tag.gpgSign", "false")
	git("commit", "--allow-empty", "-qm", "initial")
	git("tag", "v0.1.9")
	git("tag", "v0.1.10")
	git("tag", "v0.1.11-rc.1")
	git("tag", "v0.1.11")
	tag, err := previousTag(root, "v0.1.11")
	require.NoError(t, err)
	require.Equal(t, "v0.1.10", tag)
}

func TestGenerateFromConsumedCore(t *testing.T) {
	root, output := t.TempDir(), t.TempDir()
	moduleDir, err := command(filepath.Join("..", ".."), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/codefly-dev/core")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module contract-test\n\ngo 1.27.0\n\nrequire github.com/codefly-dev/core v0.0.0\nreplace github.com/codefly-dev/core => "+strings.TrimSpace(string(moduleDir))+"\n"), 0o600))
	_, err = command(root, "git", "init", "-q")
	require.NoError(t, err)
	_, err = command(root, "git", "-c", "user.name=Contract test", "-c", "user.email=contract@example.invalid", "-c", "commit.gpgSign=false", "commit", "--allow-empty", "-qm", "initial")
	require.NoError(t, err)
	require.NoError(t, generate(root, "v0.1.1", output))
	for _, name := range []string{"contract.json", "agent-requirements.json", "agent-compatibility.md"} {
		data, err := os.ReadFile(filepath.Join(output, name))
		require.NoError(t, err)
		require.Contains(t, string(data), contract.ContainerRecoveryScope)
	}
	data, err := os.ReadFile(filepath.Join(output, "contract.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), "operationContracts")
	require.NoError(t, validateManifest(data))
	data, err = os.ReadFile(filepath.Join(output, "agent-requirements.json"))
	require.NoError(t, err)
	var requirements agentv0.AgentContract
	require.NoError(t, protojson.Unmarshal(data, &requirements))
	require.Equal(t, []string{contract.ContainerRecoveryScope}, requirements.Capabilities)
	data, err = os.ReadFile(filepath.Join(output, "agent-compatibility.md"))
	require.NoError(t, err)
	require.Contains(t, string(data), "not executor advertisement")
}

func TestManifestSeparatesOperationSupportAndRejectsUnknownFields(t *testing.T) {
	data, err := protojson.Marshal(contract.Current())
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	fields["operationContracts"] = contract.SupportedOperationContracts()
	data, err = json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, validateManifest(data))
	for _, value := range []any{nil, []string{"artifact-execution/v999"}, "artifact-execution/v1"} {
		fields["operationContracts"] = value
		data, err = json.Marshal(fields)
		require.NoError(t, err)
		require.Error(t, validateManifest(data))
	}
	fields["operationContracts"] = contract.SupportedOperationContracts()
	fields["undeclaredRequirement"] = true
	data, err = json.Marshal(fields)
	require.NoError(t, err)
	require.Error(t, validateManifest(data))
}
