package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func injectionContract(t *testing.T) *environments.Environment {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "environments", "testdata", "coordinates", "config-injection.json"))
	require.NoError(t, err)
	contract, err := environments.ParseCoordinateContract(data)
	require.NoError(t, err)
	return &contract.Environment
}

func TestActualInfraBaseValuesReachRenderedWorkload(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "environments", "testdata", "coordinates", "infra-base-lodestar.json"))
	require.NoError(t, err)
	contract, err := environments.ParseCoordinateContract(data)
	require.NoError(t, err)
	env := &contract.Environment
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "accounts", "declared.example")
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "accounts"}, env))
	rendered := buildOverlay(t, root, env.Name)
	values := containerEnvironment(t, rendered)
	for key, value := range env.ServiceConfig.Services["accounts"].Values {
		require.Equal(t, value, values[key]["value"])
	}
	for _, doc := range rendered {
		require.NotEqual(t, "ServiceAccount", doc.kind, "producer declared no identity")
		require.NotEqual(t, "ExternalSecret", doc.kind, "producer declared no secret references")
	}
}

func TestConfigurationProjectionRejectsOverriddenEffectiveValues(t *testing.T) {
	for _, patch := range []string{
		"- op: replace\n  path: /spec/template/spec/containers/0/env/0/value\n  value: changed\n",
		"- op: remove\n  path: /spec/template/spec/containers/0/env\n",
		"- op: replace\n  path: /spec/template/spec/serviceAccountName\n  value: default\n",
	} {
		env := injectionContract(t)
		root := t.TempDir()
		writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
		path := filepath.Join(root, "overlays", env.Name, "kustomization.yaml")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data = append(data, []byte("patches:\n  - target:\n      kind: Deployment\n      name: api\n    patch: |-\n      "+strings.ReplaceAll(strings.TrimSpace(patch), "\n", "\n      ")+"\n")...)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		err = projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env)
		require.ErrorContains(t, err, "overlay")
	}
}

func TestIdentityProjectionRejectsEmptyPrincipal(t *testing.T) {
	env := injectionContract(t)
	env.ServiceIdentity.Default.Principal = ""
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	require.ErrorContains(t, projectManagedIdentity(t.Context(), root, &resources.Service{Name: "api"}, env), "principal")
}

func TestServiceIdentityConflictsWithManagedDependencyIdentity(t *testing.T) {
	env := injectionContract(t)
	env.ManagedServices = map[string]environments.EnvironmentManagedService{"store": managedIdentityService()}
	_, err := soleWorkloadIdentity("api", []string{"store"}, env)
	require.ErrorContains(t, err, "different runtime identities")
}

func containerEnvironment(t *testing.T, rendered []manifest) map[string]map[string]any {
	t.Helper()
	spec, ok := podSpec(manifestOfKind(t, rendered, kindDeployment))
	require.True(t, ok)
	entries := sliceField(sliceField(spec, "containers")[0].(map[string]any), "env")
	result := make(map[string]map[string]any)
	for _, item := range entries {
		entry := item.(map[string]any)
		name := entry["name"].(string)
		require.NotContains(t, result, name)
		result[name] = entry
	}
	return result
}

func TestCoordinateConfigurationAndIdentityRenderWithoutManagedServices(t *testing.T) {
	env := injectionContract(t)
	require.Empty(t, env.ManagedServices)
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env))
	rendered := buildOverlay(t, root, env.Name)
	values := containerEnvironment(t, rendered)
	require.Equal(t, "product-staging.database.example", values["DATABASE_HOST"]["value"])
	require.Equal(t, "6432", values["DATABASE_PORT"]["value"])
	require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "secret-api", "key": "DATABASE_PASSWORD"}}, values["DATABASE_PASSWORD"]["valueFrom"])
	account := manifestOfKind(t, rendered, "ServiceAccount")
	metadata := account.value["metadata"].(map[string]any)
	require.Equal(t, "product-staging-workload", metadata["annotations"].(map[string]any)["identity.example/principal"])
	secret := manifestOfKind(t, rendered, "ExternalSecret")
	data := secret.value["spec"].(map[string]any)["data"].([]any)
	require.Equal(t, map[string]any{"key": "product/api", "property": "password"}, data[0].(map[string]any)["remoteRef"])
	require.NotContains(t, values["DATABASE_PASSWORD"], "value")
}

func TestSingleServiceRenderProjectsTheSameConfiguration(t *testing.T) {
	env := injectionContract(t)
	stage := t.TempDir()
	root := filepath.Join(stage, "modules", "product", serviceUnitDir, "worker")
	writeConsumerTree(t, root, env.Name, env.Namespace, "worker", "declared.example")
	graph := map[string]*resources.Service{resources.ServiceUnique("product", "worker"): {Name: "worker"}}
	require.NoError(t, projectRenderedServiceConfiguration(t.Context(), stage, env, graph))
	rendered := buildOverlay(t, root, env.Name)
	require.Equal(t, "product-staging.database.example", containerEnvironment(t, rendered)["DATABASE_HOST"]["value"])
	account := manifestOfKind(t, rendered, "ServiceAccount")
	require.Equal(t, "product-staging-worker", account.value["metadata"].(map[string]any)["annotations"].(map[string]any)["identity.example/principal"])
}

func TestConfigurationProjectionRejectsDroppedAndConflictingBindings(t *testing.T) {
	for _, scenario := range []string{"missing container", "secret collision"} {
		t.Run(scenario, func(t *testing.T) {
			env := injectionContract(t)
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
			file := filepath.Join(root, "base", "deployment.yaml")
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(data, &doc))
			spec := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			container := spec["containers"].([]any)[0].(map[string]any)
			if scenario == "missing container" {
				container["name"] = "different"
			} else {
				container["env"] = []any{map[string]any{"name": "DATABASE_HOST", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "private", "key": "host"}}}}
			}
			original, err := yaml.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(file, original, 0o600))
			require.Error(t, projectConfigurationValues(t.Context(), root, "api", env))
			after, err := os.ReadFile(file)
			require.NoError(t, err)
			require.Equal(t, original, after)
		})
	}
}

func TestConfigurationProjectionPreservesJSONAndSkipsSidecars(t *testing.T) {
	env := injectionContract(t)
	env.ServiceSecrets = nil
	env.ServiceConfig.Services["api"].Values["SETTINGS"] = `{"nested":{"enabled":true,"limits":[1,2]},"empty":null}`
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	file := filepath.Join(root, "base", "deployment.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, yaml.Unmarshal(data, &document))
	spec := document["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	spec["containers"] = append(spec["containers"].([]any), map[string]any{"name": "sidecar", "image": "example/sidecar"})
	data, err = yaml.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0o600))
	require.NoError(t, projectConfigurationValues(t.Context(), root, "api", env))
	require.NoError(t, projectConfigurationValues(t.Context(), root, "api", env))
	rendered := buildOverlay(t, root, env.Name)
	require.JSONEq(t, env.ServiceConfig.Services["api"].Values["SETTINGS"], containerEnvironment(t, rendered)["SETTINGS"]["value"].(string))
	spec, _ = podSpec(manifestOfKind(t, rendered, kindDeployment))
	require.NotContains(t, sliceField(spec, "containers")[1].(map[string]any), "env")
}
