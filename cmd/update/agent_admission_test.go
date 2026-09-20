package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestUpdateAgentInspectsCandidateBeforeChangingSelection(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "local")
	binary := filepath.Join(t.TempDir(), "agent")
	output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../pkg/sourceworkspace/testdata/agent").CombinedOutput()
	require.NoError(t, err, "%s", output)
	candidate := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "unknown", Version: "99.0.0"}
	installed, err := candidate.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o755))
	require.NoError(t, os.Symlink(binary, installed))
	const content = "# retained comment\nname: service\nagent:\n  kind: codefly:service\n  publisher: example.test\n  name: unknown\n  version: 0.0.1\nfuture-field: retained\n"

	for _, declaration := range []string{"undeclared", "future", "unavailable", "compatible"} {
		t.Run(declaration, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			t.Setenv("TEST_SOURCE_CONTRACT", declaration)
			selected := *candidate
			selected.Version = "0.0.1"
			svc := &resources.Service{Name: "service", Agent: &selected}
			svc.WithDir(t.TempDir())
			file := filepath.Join(svc.Dir(), resources.ServiceConfigurationName)
			require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
			result, updateErr := updateServiceAgent(ctx, svc)
			after, err := os.ReadFile(file)
			require.NoError(t, err)
			if declaration == "compatible" {
				require.NoError(t, updateErr)
				require.Equal(t, &agentUpdate{Name: "unknown", From: "0.0.1", To: "99.0.0"}, result)
				require.Equal(t, "99.0.0", selected.Version)
				require.Equal(t, strings.Replace(content, "version: 0.0.1", "version: 99.0.0", 1), string(after))
				return
			}
			require.Error(t, updateErr)
			require.Nil(t, result)
			require.Equal(t, "0.0.1", selected.Version, "inspection failure must preserve the in-memory selection")
			require.Equal(t, content, string(after), "inspection failure must preserve every manifest byte")
			if declaration != "unavailable" {
				require.ErrorIs(t, updateErr, contract.ErrIncompatible)
			}
		})
	}
}
