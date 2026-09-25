package gitops

import (
	"context"
	"fmt"
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
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "accounts"}, env, scopeOf(env), serviceInjection{}))
	rendered := buildOverlay(t, root, env.Name)
	values := containerEnvironment(t, rendered)
	for key, value := range env.ServiceConfig.Services["accounts"].Values {
		require.Equal(t, value, values[key]["value"])
	}
	secretMapping := env.ServiceSecrets.Services["accounts"]
	for key := range secretMapping.RemoteKeys {
		require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "secret-accounts", "key": key}}, values[key]["valueFrom"])
	}
	seen := map[string]bool{}
	for _, doc := range rendered {
		seen[doc.kind] = true
		if doc.kind == "ServiceAccount" {
			metadata := doc.value["metadata"].(map[string]any)
			require.Equal(t, env.WorkloadIdentity("accounts").Principal, metadata["annotations"].(map[string]any)["iam.gke.io/gcp-service-account"])
		}
		if doc.kind == "ExternalSecret" {
			spec := doc.value["spec"].(map[string]any)
			require.Equal(t, "1m", spec["refreshInterval"])
			template := spec["target"].(map[string]any)["template"].(map[string]any)
			require.Equal(t, "v2", template["engineVersion"])
			require.Equal(t, "Merge", template["mergePolicy"])
			for key, expression := range secretMapping.Template.Data {
				require.Equal(t, expression, template["data"].(map[string]any)[key])
			}
		}
	}
	require.True(t, seen["ServiceAccount"])
	require.True(t, seen["ExternalSecret"])
	require.Empty(t, env.ManagedServices)
	addProjectionPatch(t, root, env.Name, "ExternalSecret", "- op: replace\n  path: /spec/target/template/data/SOLUTION_REGISTRATION_SECRETS\n  value: wrong-value")
	require.ErrorContains(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "accounts"}, env, scopeOf(env), serviceInjection{}), "ExternalSecret")
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
		err = projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{})
		require.ErrorContains(t, err, "overlay")
	}
}

func TestConfigurationProjectionRejectsRedirectedSecretDelivery(t *testing.T) {
	for name, patch := range map[string]string{
		"removed":           "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\nmetadata:\n  name: secret-api\n$patch: delete",
		"remote key":        "- op: replace\n  path: /spec/data/0/remoteRef/key\n  value: other-tenant/api",
		"property":          "- op: replace\n  path: /spec/data/0/remoteRef/property\n  value: other",
		"store":             "- op: replace\n  path: /spec/secretStoreRef/name\n  value: other-store",
		"target":            "- op: replace\n  path: /spec/target/name\n  value: other-secret",
		"namespace":         "- op: replace\n  path: /metadata/namespace\n  value: other-namespace",
		"missing key":       "- op: remove\n  path: /spec/data/0",
		"template override": "- op: add\n  path: /spec/target/template\n  value:\n    data:\n      DATABASE_PASSWORD: wrong-value",
		"extra source":      "- op: add\n  path: /spec/dataFrom\n  value:\n    - extract:\n        key: other-tenant/api",
	} {
		t.Run(name, func(t *testing.T) {
			env := injectionContract(t)
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
			addProjectionPatch(t, root, env.Name, "ExternalSecret", patch)
			require.ErrorContains(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}), "ExternalSecret")
		})
	}
}

func TestConfigurationProjectionRejectsConsumerNamespaceMismatch(t *testing.T) {
	env := injectionContract(t)
	env.ServiceIdentity = nil
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	addProjectionPatch(t, root, env.Name, "Deployment", "- op: replace\n  path: /metadata/namespace\n  value: other-namespace")
	require.ErrorContains(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}), "different namespace")
}

func TestConfigurationProjectionChecksDiscoveredSecretDelivery(t *testing.T) {
	env := injectionContract(t)
	env.ServiceConfig, env.ServiceIdentity = nil, nil
	env.ServiceSecrets.Services = nil
	root := t.TempDir()
	writeServiceTreeReferencingKeys(t, root, env.Name, env.Namespace, "api", []string{"PASSWORD"})
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}))
	addProjectionPatch(t, root, env.Name, "ExternalSecret", "- op: replace\n  path: /spec/data/0/remoteRef/key\n  value: other-tenant/api")
	require.ErrorContains(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}), "ExternalSecret")
}

func TestIdentityProjectionRejectsOverlayNamespaceMismatch(t *testing.T) {
	env := injectionContract(t)
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	addProjectionPatch(t, root, env.Name, "ServiceAccount", "- op: replace\n  path: /metadata/namespace\n  value: wrong-namespace")
	require.ErrorContains(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}), "service account product/api")
}

func TestConfigurationScalarSpellingsReachRenderedWorkload(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "environments", "testdata", "scalar-workspace.yaml"))
	require.NoError(t, err)
	workspace, err := resources.LoadFromBytes[resources.Workspace](data)
	require.NoError(t, err)
	env, err := environments.Select(workspace, "staging")
	require.NoError(t, err)
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}))
	values := containerEnvironment(t, buildOverlay(t, root, env.Name))
	for key, expected := range map[string]string{"ACCOUNT": "00123", "DATE": "2026-09-21", "EXPONENT": "1e3"} {
		require.Equal(t, expected, values[key]["value"])
	}
}

func addProjectionPatch(t *testing.T, root, environment, kind, patch string) {
	t.Helper()
	file := filepath.Join(root, "overlays", environment, "kustomization.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, yaml.Unmarshal(data, &document))
	document["patches"] = append(sliceField(document, "patches"), map[string]any{"target": map[string]any{"kind": kind}, "patch": patch})
	data, err = yaml.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0o600))
}

func TestConfigurationProjectionRejectsMissingAndCompetingExternalSecrets(t *testing.T) {
	for _, scenario := range []string{"missing", "competing"} {
		t.Run(scenario, func(t *testing.T) {
			env := injectionContract(t)
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
			require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}))
			file := filepath.Join(root, "overlays", env.Name, "external-secret.yaml")
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			var document map[string]any
			require.NoError(t, yaml.Unmarshal(data, &document))
			if scenario == "missing" {
				mapField(document, "spec")["target"] = map[string]any{"name": "elsewhere"}
			} else {
				mapField(document, "metadata")["name"] = "competing"
				file = filepath.Join(root, "overlays", env.Name, "competing.yaml")
				require.NoError(t, addKustomizationResource(filepath.Dir(file), filepath.Base(file)))
			}
			data, err = yaml.Marshal(document)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(file, data, 0o600))
			require.ErrorContains(t, validateProjectedConfiguration(root, &resources.Service{Name: "api"}, env, scopeOf(env)), "exactly one projected ExternalSecret")
		})
	}
}

func TestIdentityProjectionRejectsEmptyPrincipal(t *testing.T) {
	env := injectionContract(t)
	env.ServiceIdentity.Default.Principal = ""
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	require.ErrorContains(t, projectManagedIdentity(t.Context(), root, &resources.Service{Name: "api"}, env, env.Namespace), "principal")
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
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}))
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
	require.NoError(t, projectRenderedServiceConfiguration(t.Context(), stage, singleModuleWorkspace(), env, graph, nil, nil))
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
				delete(container, "envFrom")
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

func TestConfigurationProjectionUsesDeclaredServiceNotContainerName(t *testing.T) {
	for _, name := range []string{"nextjs", "postgres", "redis", "object-storage", "arbitrary-container"} {
		t.Run(name, func(t *testing.T) {
			env := injectionContract(t)
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
			file := filepath.Join(root, "base", "deployment.yaml")
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(data, &doc))
			spec, ok := podSpec(manifest{kind: kindDeployment, value: doc})
			require.True(t, ok)
			container := sliceField(spec, "containers")[0].(map[string]any)
			container["name"] = name
			// A name match is not permission to receive the application's secrets.
			spec["containers"] = append(sliceField(spec, "containers"), map[string]any{"name": "api", "image": "example/sidecar"})
			data, err = yaml.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(file, data, 0o600))
			require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}))
			rendered := buildOverlay(t, root, env.Name)
			require.Equal(t, "product-staging.database.example", containerEnvironment(t, rendered)["DATABASE_HOST"]["value"])
			spec, _ = podSpec(manifestOfKind(t, rendered, kindDeployment))
			require.NotContains(t, sliceField(spec, "containers")[1].(map[string]any), "env")
		})
	}
}

func TestIdentityProjectionDoesNotRequireAContainerBinding(t *testing.T) {
	env := injectionContract(t)
	env.ServiceConfig, env.ServiceSecrets = nil, nil
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
	file := filepath.Join(root, "base", "deployment.yaml")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	data = []byte(strings.Replace(string(data), "- name: api", "- name: nextjs", 1))
	data = []byte(strings.Replace(string(data), "          envFrom:\n            - configMapRef:\n                name: api\n", "", 1))
	require.NoError(t, os.WriteFile(file, data, 0o600))
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}))
	require.Equal(t, "api", mapField(podTemplate(manifestOfKind(t, buildOverlay(t, root, env.Name), kindDeployment)), "spec")["serviceAccountName"])
}

func TestIdentityProjectionRejectsServiceAccountInAnotherNamespace(t *testing.T) {
	for _, existingAccount := range []bool{false, true} {
		t.Run(fmt.Sprint(existingAccount), func(t *testing.T) {
			env := injectionContract(t)
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "api", "declared.example")
			require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{}))
			file := filepath.Join(root, "base", "serviceaccount.yaml")
			raw, err := os.ReadFile(file)
			require.NoError(t, err)
			var account map[string]any
			require.NoError(t, yaml.Unmarshal(raw, &account))
			mapField(account, "metadata")["namespace"] = "wrong-namespace"
			raw, err = yaml.Marshal(account)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(file, raw, 0o600))
			if existingAccount {
				// The same-named account the pod can actually reach lacks the identity.
				overlay := filepath.Join(root, "overlays", env.Name)
				data := []byte("apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: api\n  namespace: " + env.Namespace + "\n")
				require.NoError(t, os.WriteFile(filepath.Join(overlay, "other-account.yaml"), data, 0o600))
				require.NoError(t, addKustomizationResource(overlay, "other-account.yaml"))
			}
			require.ErrorContains(t, validateProjectedConfiguration(root, &resources.Service{Name: "api"}, env, scopeOf(env)), "service account")
		})
	}
}

func TestRejectedConfigurationCannotReplacePublishedTree(t *testing.T) {
	env := injectionContract(t)
	destination := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(destination, "retained.yaml"), []byte("original"), 0o600))
	_, err := RenderOwnedTree(t.Context(), &RenderOptions{Destination: destination, Promotable: true}, func(ctx context.Context, stage string) error {
		writeConsumerTree(t, stage, env.Name, env.Namespace, "api", "declared.example")
		addProjectionPatch(t, stage, env.Name, "ExternalSecret", "- op: replace\n  path: /spec/data/0/remoteRef/key\n  value: other-tenant/api")
		return projectServiceConfiguration(ctx, stage, &resources.Service{Name: "api"}, env, scopeOf(env), serviceInjection{})
	})
	require.ErrorContains(t, err, "ExternalSecret")
	data, err := os.ReadFile(filepath.Join(destination, "retained.yaml"))
	require.NoError(t, err)
	require.Equal(t, "original", string(data))
	entries, err := os.ReadDir(destination)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
