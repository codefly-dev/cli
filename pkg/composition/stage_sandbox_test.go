//go:build sandbox_e2e

package composition

import (
	"testing"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/runners/sandbox"
	"github.com/stretchr/testify/require"
)

func TestStageRenderWithRealSandboxAndUDS(t *testing.T) {
	session, files, options := stageFixture(t)
	options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
		sb, err := sandbox.New()
		if err != nil {
			return nil, err
		}
		require.NotEqual(t, sandbox.BackendNative, sb.Backend())
		sb.WithWritePaths(directory).WithNetwork(sandbox.NetworkDeny)
		return []manager.LoadOption{manager.WithSandbox(sb), manager.WithoutPrincipal(), manager.WithUDS(), manager.WithWorkDir(directory)}, nil
	}
	result, err := session.StageRender(t.Context(), files, options)
	require.NoError(t, err)
	require.Len(t, result.Record.Executions, 4)
}

func TestStageBuildWithRealSandboxAndUDS(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
		sb, err := sandbox.New()
		if err != nil {
			return nil, err
		}
		require.NotEqual(t, sandbox.BackendNative, sb.Backend())
		// This real source packager must fetch the exact source URI over TLS.
		sb.WithWritePaths(directory).WithNetwork(sandbox.NetworkOpen)
		return []manager.LoadOption{manager.WithSandbox(sb), manager.WithoutPrincipal(), manager.WithUDS(), manager.WithWorkDir(directory)}, nil
	}
	result, err := session.StageBuild(t.Context(), options)
	require.NoError(t, err)
	require.Len(t, result.Executions, 2)
}
