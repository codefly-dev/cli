package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestSolutionExecutorAdmissionBeforeExposingClient(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "local")
	binary := filepath.Join(t.TempDir(), "agent")
	output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../sourceworkspace/testdata/agent").CombinedOutput()
	require.NoError(t, err, "%s", output)
	// Exercise the live RPC boundary with a real process. Full solution-kind
	// loading is separately blocked by Core #589's provider-only artifact verifier.
	selected := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "unknown", Version: "99.0.0"}
	installed, err := selected.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o755))
	require.NoError(t, os.Symlink(binary, installed))

	for _, declaration := range []string{"undeclared", "future", "unavailable", "compatible"} {
		t.Run(declaration, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			t.Setenv("TEST_SOURCE_CONTRACT", declaration)
			conn, err := manager.Load(ctx, selected, manager.WithoutSandbox(), manager.WithoutPrincipal())
			require.NoError(t, err)
			defer conn.Close()
			client, err := admittedSolutionExecutor(ctx, conn)
			if declaration == "compatible" {
				require.NoError(t, err)
				require.NotNil(t, client)
				return
			}
			require.Error(t, err)
			require.Nil(t, client, "a rejected peer must never receive Package or Render")
			if declaration != "unavailable" {
				require.ErrorIs(t, err, contract.ErrIncompatible)
			}
		})
	}
}
