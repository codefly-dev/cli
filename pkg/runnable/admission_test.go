package runnable

import (
	"context"
	"io"
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

func TestRunnableAdmissionPrecedesBuilderLoad(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "local")
	binary := filepath.Join(t.TempDir(), "agent")
	output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../sourceworkspace/testdata/agent").CombinedOutput()
	require.NoError(t, err, "%s", output)
	selected := &resources.Agent{Kind: resources.RunnableAgent, Publisher: "example.test", Name: "unknown", Version: "0.0.1"}
	installed, err := selected.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o755))
	require.NoError(t, os.Symlink(binary, installed))

	for _, declaration := range []string{"undeclared", "future", "unavailable", "no-builder", "compatible"} {
		t.Run(declaration, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			t.Setenv("TEST_SOURCE_CONTRACT", declaration)
			t.Setenv("TEST_SOURCE_NO_BUILDER", "")
			if declaration == "no-builder" {
				t.Setenv("TEST_SOURCE_NO_BUILDER", "1")
			}
			marker := filepath.Join(t.TempDir(), "loaded")
			t.Setenv("TEST_SOURCE_BUILDER_LOADED", marker)
			workspace := &resources.Workspace{Name: "test"}
			workspace.WithDir(t.TempDir())
			r, err := resources.NewRunnable(ctx, "task", selected, "handler.go")
			require.NoError(t, err)
			r.WithDir(filepath.Join(workspace.Dir(), "runnables", "task"))
			r.SetModule("test")
			client, closeAgent, err := load(ctx, workspace, r, io.Discard)
			if closeAgent != nil {
				defer closeAgent()
			}
			if declaration == "compatible" {
				require.NoError(t, err)
				require.NotNil(t, client)
				require.FileExists(t, marker)
				return
			}
			require.Error(t, err)
			require.Nil(t, client)
			require.Nil(t, closeAgent)
			require.NoFileExists(t, marker, "rejected peers must never reach Builder.Load")
			if declaration == "undeclared" || declaration == "future" {
				require.ErrorIs(t, err, contract.ErrIncompatible)
			}
			if declaration == "no-builder" {
				require.ErrorContains(t, err, "does not advertise Builder")
			}
		})
	}
}
