package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	civ0 "github.com/codefly-dev/core/generated/go/codefly/ci/v0"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestAgentCIKeepsOnePrivateSourceSelection(t *testing.T) {
	binDir := t.TempDir()
	cli := filepath.Join(binDir, "codefly")
	peer := filepath.Join(binDir, "peer")
	for output, source := range map[string]string{cli: "../codefly", peer: "../../pkg/sourceworkspace/testdata/agent"} {
		command := exec.CommandContext(t.Context(), "go", "build", "-o", output, source)
		command.Env = append(os.Environ(), "GOWORK=off")
		data, err := command.CombinedOutput()
		require.NoError(t, err, "%s", data)
	}
	peerBytes, err := os.ReadFile(peer)
	require.NoError(t, err)
	for _, selection := range []string{"implicit", "older", "latest", "self"} {
		t.Run(selection, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			manifest := "publisher: example.test\nkind: codefly:service\nname: candidate\nversion: 9.0.0\n"
			selected := "packager__1.0.0"
			switch selection {
			case "older":
				manifest += "source:\n  directory: .\n  agent: example.test/packager:1.0.0\n"
			case "latest":
				selected = "packager__2.0.0"
				manifest += "source:\n  directory: .\n  agent: example.test/packager:latest\n"
			case "self":
				selected = "candidate__9.0.0"
				manifest += "source:\n  directory: .\n  agent: self\n  bootstrap: [sh, ./bootstrap.sh]\n"
			}
			installed := filepath.Join(home, "agents", "services", "example.test", "packager__1.0.0")
			if selection != "self" {
				require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o700))
				require.NoError(t, os.WriteFile(installed, peerBytes, 0o700))
			}
			if selection == "older" {
				// A latest-only inventory would lose the usable explicitly selected peer.
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(installed), "packager__2.0.0"), []byte("not the selected executable"), 0o700))
			}
			if selection == "latest" {
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(installed), "packager__2.0.0"), peerBytes, 0o700))
			}
			marker, calls := filepath.Join(t.TempDir(), "bootstrap"), filepath.Join(t.TempDir(), "calls")
			require.NoError(t, os.WriteFile(filepath.Join(root, "agent.codefly.yaml"), []byte(manifest), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/candidate\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "bootstrap.sh"), []byte("printf 'bootstrap\n' >> \"$CI_BOOTSTRAP_MARKER\"\ncp \"$CI_SOURCE_PEER\" \"$CODEFLY_AGENT_OUTPUT\"\n"), 0o700))
			data, err := exec.CommandContext(t.Context(), "git", "init", root).CombinedOutput()
			require.NoError(t, err, "%s", data)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, cli, "agent", "ci", "--dir", root, "--native-only", "--skip-audit", "--skip-conformance", "--format", "json")
			command.Env = agentCIChildEnvironment(home, "TEST_SOURCE_CI_RECORD="+calls, "CI_SOURCE_PEER="+peer, "CI_BOOTSTRAP_MARKER="+marker,
				"CODEFLY_AGENT_SOURCE=local", "AGENT_REGISTRY=", "AGENT_NIX_FLAKE=", "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1")
			data, err = command.CombinedOutput()
			require.Error(t, err, "controlled Package refusal must be reported")
			require.NoError(t, ctx.Err(), "%s", data)
			var report civ0.AgentCIReport
			require.NoError(t, protojson.Unmarshal(data, &report), "%s", data)
			require.Equal(t, "passed", report.Stages[1].Status, "%s", data)
			require.Equal(t, "failed", report.Stages[2].Status, "%s", data)
			require.Contains(t, report.Stages[2].GetError(), "CI package boundary reached")
			recorded, err := os.ReadFile(calls)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
			require.Len(t, lines, 2, "%s", recorded)
			testPath := strings.TrimPrefix(lines[0], "Runtime.Test ")
			packagePath := strings.TrimPrefix(lines[1], "Builder.Package ")
			require.Equal(t, testPath, packagePath)
			require.Equal(t, selected, filepath.Base(testPath))
			require.NotContains(t, testPath, home)
			_, err = os.Stat(testPath)
			require.ErrorIs(t, err, os.ErrNotExist, "private selection must be cleaned after CI failure")
			if selection == "self" {
				bootstrap, err := os.ReadFile(marker)
				require.NoError(t, err)
				require.Equal(t, "bootstrap\n", string(bootstrap), "bootstrap must run exactly once")
			} else {
				original, err := os.ReadFile(installed)
				require.NoError(t, err)
				require.Equal(t, peerBytes, original, "CI must not replace the user's executable")
			}
		})
	}
}

// buildAgentCIFixtures builds the CLI under test and the recording source agent
// that stands in for a real packager.
func buildAgentCIFixtures(t *testing.T) (string, string, []byte) {
	t.Helper()
	binDir := t.TempDir()
	cli := filepath.Join(binDir, "codefly")
	peer := filepath.Join(binDir, "peer")
	for output, source := range map[string]string{cli: "../codefly", peer: "../../pkg/sourceworkspace/testdata/agent"} {
		command := exec.CommandContext(t.Context(), "go", "build", "-o", output, source)
		command.Env = append(os.Environ(), "GOWORK=off")
		data, err := command.CombinedOutput()
		require.NoError(t, err, "%s", data)
	}
	peerBytes, err := os.ReadFile(peer)
	require.NoError(t, err)
	return cli, peer, peerBytes
}

// writeAgentCICandidate lays out a candidate repository whose manifest selects
// its source packager the way selection names, installs the packagers that
// selection expects in home, and answers the executable CI must keep using.
func writeAgentCICandidate(t *testing.T, root, home string, peerBytes []byte, selection string) string {
	t.Helper()
	manifest := "publisher: example.test\nkind: codefly:service\nname: candidate\nversion: 9.0.0\n"
	selected := "packager__1.0.0"
	switch selection {
	case "older":
		manifest += "source:\n  directory: .\n  agent: example.test/packager:1.0.0\n"
	case "latest":
		selected = "packager__2.0.0"
		manifest += "source:\n  directory: .\n  agent: example.test/packager:latest\n"
	case "self":
		selected = "candidate__9.0.0"
		manifest += "source:\n  directory: .\n  agent: self\n  bootstrap: [sh, ./bootstrap.sh]\n"
	}
	installed := filepath.Join(home, "agents", "services", "example.test", "packager__1.0.0")
	if selection != "self" {
		require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o700))
		require.NoError(t, os.WriteFile(installed, peerBytes, 0o700))
	}
	if selection == "latest" {
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(installed), "packager__2.0.0"), peerBytes, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "agent.codefly.yaml"), []byte(manifest), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/candidate\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bootstrap.sh"), []byte("cp \"$CI_SOURCE_PEER\" \"$CODEFLY_AGENT_OUTPUT\"\n"), 0o700))
	data, err := exec.CommandContext(t.Context(), "git", "init", root).CombinedOutput()
	require.NoError(t, err, "%s", data)
	return selected
}

// runAgentCIFixture runs a complete agent CI and returns the report alongside
// the plugin calls the recording agent observed, in order. A run that fails
// before reaching the agent records nothing; the report carries why.
func runAgentCIFixture(t *testing.T, cli, root, home, peer string, environment ...string) (*civ0.AgentCIReport, []string, error) {
	t.Helper()
	calls := filepath.Join(t.TempDir(), "calls")
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, cli, "agent", "ci", "--dir", root, "--native-only", "--skip-conformance", "--output", t.TempDir(), "--format", "json")
	command.Env = agentCIChildEnvironment(home, append(environment,
		"TEST_SOURCE_CI_RECORD="+calls, "CI_SOURCE_PEER="+peer, "TEST_SOURCE_PACKAGE_EXECUTABLE="+peer,
		"CODEFLY_AGENT_SOURCE=local", "AGENT_REGISTRY=", "AGENT_NIX_FLAKE=", "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1")...)
	data, runErr := command.CombinedOutput()
	require.NoError(t, ctx.Err(), "%s", data)
	var report civ0.AgentCIReport
	require.NoError(t, protojson.Unmarshal(data, &report), "%s", data)
	recorded, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return &report, nil, runErr
	}
	require.NoError(t, err)
	return &report, strings.Split(strings.TrimSpace(string(recorded)), "\n"), runErr
}

// The audit stage runs after CI isolates CODEFLY_HOME to the candidate's own
// home, which holds the candidate alone. Rediscovering a source agent there
// would audit through the candidate instead of the selected packager, and
// reaching the right packager is only half the gate: the release policy must
// still block on what that packager reports.
func TestAgentCIAuditStage(t *testing.T) {
	cli, peer, peerBytes := buildAgentCIFixtures(t)
	for _, selection := range []string{"implicit", "older", "latest", "self"} {
		t.Run(selection, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			selected := writeAgentCICandidate(t, root, home, peerBytes, selection)
			report, calls, err := runAgentCIFixture(t, cli, root, home, peer)
			require.NoError(t, err)
			require.Equal(t, "passed", report.GetStatus())
			require.Equal(t, "audit", report.Stages[3].GetName())
			require.Equal(t, "passed", report.Stages[3].GetStatus())
			require.Len(t, calls, 3, "%v", calls)
			testPath := strings.TrimPrefix(calls[0], "Runtime.Test ")
			auditPath := strings.TrimPrefix(calls[2], "Builder.Audit ")
			require.Equal(t, testPath, strings.TrimPrefix(calls[1], "Builder.Package "))
			require.Equal(t, testPath, auditPath, "audit must reuse the validated and packaging selection")
			require.Equal(t, selected, filepath.Base(auditPath))
			require.NotContains(t, auditPath, home)
		})
	}
	t.Run("gates on actionable vulnerabilities", func(t *testing.T) {
		root, home := t.TempDir(), t.TempDir()
		writeAgentCICandidate(t, root, home, peerBytes, "older")
		report, calls, err := runAgentCIFixture(t, cli, root, home, peer, "TEST_SOURCE_AUDIT_VULNERABLE=1")
		require.Error(t, err, "an actionable HIGH finding must fail agent CI")
		require.Equal(t, "failed", report.GetStatus())
		require.Equal(t, "audit", report.Stages[3].GetName())
		require.Equal(t, "failed", report.Stages[3].GetStatus())
		require.Contains(t, report.Stages[3].GetError(), "high/critical")
		require.Len(t, calls, 3, "%v", calls)
		require.Equal(t, "packager__1.0.0", filepath.Base(strings.TrimPrefix(calls[2], "Builder.Audit ")))
	})
}
