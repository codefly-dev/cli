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
