package validation

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/stretchr/testify/require"
)

func TestValidationRequiresRuntimeDependencyBeforeTarget(t *testing.T) {
	for _, mode := range []orchestration.Mode{orchestration.LintMode, orchestration.CompileMode} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
			t.Setenv(manager.AgentSourceEnv, "local")
			t.Setenv(recoveryscope.EnvironmentVariable, "")
			root := t.TempDir()
			require.NoError(t, os.CopyFS(root, os.DirFS("testdata/dependency")))
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
			require.NoError(t, err)
			module, err := workspace.LoadModuleFromName(ctx, "app")
			require.NoError(t, err)
			service, err := module.LoadServiceFromName(ctx, "consumer")
			require.NoError(t, err)
			err = RunServiceWithOptions(ctx, workspace, module, service, mode, string(mode), resources.RuntimeContextNative, Options{Disposable: true})
			require.Error(t, err)
			require.Contains(t, err.Error(), "missing-prerequisite",
				"validation must resolve the runtime prerequisite before initializing its consumer")
		})
	}
}
