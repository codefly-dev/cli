package gitops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestConfigurationBindingFollowsRuntimeIdentityDeclarations(t *testing.T) {
	for name, fields := range map[string]string{
		"literal":                    "env:\n  - name: CODEFLY__SERVICE\n    value: api\n",
		"config map key":             "env:\n  - name: CODEFLY__SERVICE\n    valueFrom:\n      configMapKeyRef:\n        name: api\n        key: CODEFLY__SERVICE\n",
		"envFrom":                    "envFrom:\n  - configMapRef:\n      name: api\n",
		"explicit overrides envFrom": "envFrom:\n  - secretRef:\n      name: unresolved\nenv:\n  - name: CODEFLY__SERVICE\n    value: api\n",
	} {
		t.Run(name, func(t *testing.T) {
			env := injectionContract(t)
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
			file := filepath.Join(root, "base", "deployment.yaml")
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(data, &doc))
			spec, _ := podSpec(manifest{kind: kindDeployment, value: doc})
			container := sliceField(spec, "containers")[0].(map[string]any)
			container["name"] = "agent-chosen"
			delete(container, "envFrom")
			var declared map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(fields), &declared))
			for key, value := range declared {
				container[key] = value
			}
			data, err = yaml.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(file, data, 0o600))
			require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env)))
			require.Equal(t, "product-staging.database.example", containerEnvironment(t, buildOverlay(t, root, env.Name))["DATABASE_HOST"]["value"])
		})
	}
}

func TestConfigurationBindingUsesOnlySelectedResources(t *testing.T) {
	env := injectionContract(t)
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	other := filepath.Join(root, "overlays", "unselected")
	require.NoError(t, os.Mkdir(other, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(other, "config.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\n  namespace: product\ndata:\n  CODEFLY__SERVICE: another-service\n"), 0o600))
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env)))
}

func TestConfigurationBindingSurvivesKustomizeNameTransforms(t *testing.T) {
	env := injectionContract(t)
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	file := filepath.Join(root, "overlays", env.Name, "kustomization.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	data = append(data, []byte("namePrefix: prefixed-\n")...)
	require.NoError(t, os.WriteFile(file, data, 0o600))
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env)))
}

func TestConfigurationBindingRejectsAnEffectiveIdentityOverride(t *testing.T) {
	env := injectionContract(t)
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	addProjectionPatch(t, root, env.Name, "ConfigMap", "- op: replace\n  path: /data/CODEFLY__SERVICE\n  value: another-service")
	require.ErrorContains(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env)), "bind no effective workload")
}

func TestConfigurationBindingDoesNotResolveAcrossNamespaces(t *testing.T) {
	docs, _, err := decodeYAML("config.yaml", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api\n  namespace: another-namespace\ndata:\n  CODEFLY__SERVICE: api\n"))
	require.NoError(t, err)
	index, err := indexConfigurationMaps(docs)
	require.NoError(t, err)
	var container map[string]any
	require.NoError(t, yaml.Unmarshal([]byte("name: api\nenvFrom:\n  - configMapRef:\n      name: api\n"), &container))
	service, err := index.service(container, "product")
	require.NoError(t, err)
	require.Empty(t, service)
}
