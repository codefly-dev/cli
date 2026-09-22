package composition

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestTargetCommandHasOfflineHelpAndRequiresExplicitEnvironment(t *testing.T) {
	command := NewCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"inspect-local-target", "--help"})
	require.NoError(t, command.Execute())
	require.Contains(t, output.String(), "--expected-identity")
	require.Contains(t, output.String(), "does not qualify")
	workspace := t.TempDir()
	data, err := yaml.Marshal(map[string]any{"name": "target-inspection", "layout": "modules", "environments": []*environments.Environment{
		{Name: "missing-namespace", Cluster: &environments.EnvironmentCluster{Kind: "k3d"}},
		{Name: "remote", Namespace: "product", Cluster: &environments.EnvironmentCluster{Kind: "remote"}},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, resources.WorkspaceConfigurationName), data, 0o600))
	for _, test := range []struct{ environment, message string }{
		{"undeclared", "does not declare"}, {"missing-namespace", "explicit valid environment namespace"}, {"remote", "exact local k3d target"},
	} {
		command = NewCommand()
		output.Reset()
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetArgs([]string{"--workspace", workspace, "inspect-local-target", test.environment})
		require.ErrorContains(t, command.Execute(), test.message)
		require.NotContains(t, output.String(), `"identity"`)
	}
}
