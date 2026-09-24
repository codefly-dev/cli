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
	_, err := environments.FromRuntime(&resources.Environment{Name: "production", Extensions: map[string]resources.YAMLValue{"service-confg": {Node: yaml.Node{Kind: yaml.MappingNode}}}})
	require.ErrorContains(t, err, "service-confg")
}

func TestSelectionPreservesScalarSpellings(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "scalar-workspace.yaml"))
	require.NoError(t, err)
	workspace, err := resources.LoadFromBytes[resources.Workspace](data)
	require.NoError(t, err)
	selected, err := environments.Select(workspace, "staging")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"ACCOUNT": "00123", "DATE": "2026-09-21", "EXPONENT": "1e3"}, selected.ServiceConfig.Services["api"].Values)
	require.Equal(t, "00123", selected.ServiceSecrets.Services["api"].RemoteKeys["PASSWORD"].Key)
	require.Equal(t, "00123", selected.ServiceIdentity.Default.Annotations["identity.example/account"])
	gitops, err := environments.WorkspaceGitops(workspace)
	require.NoError(t, err)
	require.Equal(t, "00123", gitops.Branch)
	resource, err := selected.Resource()
	require.NoError(t, err)
	again, err := environments.FromRuntime(resource)
	require.NoError(t, err)
	require.Equal(t, selected, again)
}

// A declared profile chain is admitted and reaches Core's runtime environment,
// which reads each configuration location through it.
func TestSelectionCarriesTheConfigurationProfileChain(t *testing.T) {
	var workspace resources.Workspace
	require.NoError(t, yaml.Unmarshal([]byte(`name: product
environments:
  - name: staging
    configuration-profiles: [staging, local]
`), &workspace))
	env, err := environments.Select(&workspace, "staging")
	require.NoError(t, err)
	require.Equal(t, []string{"staging", "local"}, env.ConfigurationProfiles)
	names, err := env.Runtime().ConfigurationProfileNames()
	require.NoError(t, err)
	require.Equal(t, []string{"staging", "local"}, names)
	own, err := env.ConfigurationProfileName()
	require.NoError(t, err)
	require.Equal(t, "staging", own)
}
