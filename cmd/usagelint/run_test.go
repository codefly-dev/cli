package usagelint

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestRunChecksSourceWithoutRuntime(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv("TEST_SOURCE_USAGE", advertised(t))
	agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "source-only", Version: "1.0.0"}
	binary, err := agent.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(binary), 0o700))
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../pkg/sourceworkspace/testdata/agent")
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)

	service := &resources.Service{
		Name: "worker", Version: "0.0.0", Agent: agent,
		ServiceDependencies: []*resources.ServiceDependency{
			{Name: "upstream", Module: "external-module", Kind: resources.DependencyKindExternal},
		},
	}
	service.WithDir(t.TempDir())
	service.WithModule("app")
	require.NoError(t, service.Save(t.Context()))

	report, err := Run(t.Context(), &resources.Workspace{}, &resources.Module{Name: "app"}, service)
	require.NoError(t, err)
	require.Len(t, report.Owners, 1)
	require.Empty(t, report.Incomplete)
}
