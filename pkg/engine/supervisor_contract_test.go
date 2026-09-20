package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestSupervisorChecksExplicitSelectionBeforeCreatingSession(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	binary := filepath.Join(t.TempDir(), "agent")
	output, err := exec.Command("go", "build", "-o", binary, "../sourceworkspace/testdata/agent").CombinedOutput()
	require.NoError(t, err, "%s", output)
	selected := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "unknown", Version: "0.0.1"}
	installed, err := selected.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o755))
	require.NoError(t, os.Symlink(binary, installed))
	for _, declaration := range []string{"future", "undeclared", ""} {
		t.Run("contract="+declaration, func(t *testing.T) {
			t.Setenv("TEST_SOURCE_CONTRACT", declaration)
			root := t.TempDir()
			supervisor := NewAgentSupervisor(AgentSupervisorConfig{Root: root})
			defer supervisor.Close()
			session, err := supervisor.acquire(t.Context(), ServiceTarget{Root: root, Agent: selected.Identifier(), ForceSource: true})
			if declaration != "" {
				require.ErrorIs(t, err, contract.ErrIncompatible)
				require.Nil(t, session)
				require.Empty(t, supervisor.sessions)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, session)
			require.False(t, session.runtimeOK)
		})
	}
}
