package environments_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSelectionDoesNotShareDeploymentMaps(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "coordinates", "config-injection.json"))
	require.NoError(t, err)
	contract, err := environments.ParseCoordinateContract(data)
	require.NoError(t, err)
	resource, err := contract.Environment.Resource()
	require.NoError(t, err)
	workspace := &resources.Workspace{Environments: []*resources.Environment{resource}}
	before, err := yaml.Marshal(workspace)
	require.NoError(t, err)
	first, err := environments.Select(workspace, resource.Name)
	require.NoError(t, err)
	first.ServiceConfig.Services["api"].Values["DATABASE_HOST"] = "changed"
	first.ServiceIdentity.Default.Annotations["changed"] = "yes"
	first.ServiceIdentity.Services["worker"].Labels["changed"] = "yes"
	first.ServiceSecrets.Services["api"].RemoteKeys["DATABASE_PASSWORD"] = environments.EnvironmentSecretRemoteRef{Key: "changed"}
	after, err := yaml.Marshal(workspace)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	second, err := environments.Select(workspace, resource.Name)
	require.NoError(t, err)
	require.Equal(t, contract.Environment, *second)
	require.Empty(t, second.Runtime().Extensions)
}

func TestRuntimeExtensionsAreAdmittedByCLI(t *testing.T) {
	_, err := environments.FromRuntime(&resources.Environment{Name: "production", Extensions: map[string]any{"service-confg": map[string]any{}}})
	require.ErrorContains(t, err, "service-confg")
}
