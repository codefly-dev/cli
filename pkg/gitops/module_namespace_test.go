package gitops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// writeTwoModuleWorkspace lays down a modules-layout workspace composing "saas"
// and "documents", each a service-less module with a kustomize bootstrap, and a
// staging environment declaring namespace "platform" and a resource quota.
func writeTwoModuleWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: acme
layout: modules
modules:
  - name: saas
  - name: documents
environments:
  - name: staging
    namespace: platform
    resource-quota:
      requests:
        cpu: "4"
        memory: 8Gi
`,
	}
	for _, module := range []string{"saas", "documents"} {
		files[filepath.Join("modules", module, resources.ModuleConfigurationName)] = "kind: module\nname: " + module + "\n"
		files[filepath.Join("modules", module, "deployment", "kustomize", "base", "kustomization.yaml")] = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"
		files[filepath.Join("modules", module, "deployment", "kustomize", "base", "deployment.yaml")] = pinnedDeployment
		files[filepath.Join("modules", module, "deployment", "kustomize", "overlays", "staging", "kustomization.yaml")] = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	return workspace
}

// A workspace composing several modules renders each into its own namespace,
// "<namespace>-<module>", and every projection that names the namespace agrees:
// the render record, the quota projected into the bootstrap, and the Argo
// AppProject destination derived from the record at publish time.
func TestRenderModuleScopesNamespacePerModule(t *testing.T) {
	ctx := context.Background()
	workspace := writeTwoModuleWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NotNil(t, env)

	for module, namespace := range map[string]string{"saas": "platform-saas", "documents": "platform-documents"} {
		loaded, err := workspace.LoadModuleFromName(ctx, module)
		require.NoError(t, err)
		result, err := RenderModule(ctx, workspace, loaded, env, "acme-staging", nil)
		require.NoError(t, err, "render %s", module)
		require.Equal(t, namespace, result.Inventory.Namespace)

		recorded, err := os.ReadFile(filepath.Join(result.Path, InventoryFilename))
		require.NoError(t, err)
		var record struct {
			Namespace string `json:"namespace"`
		}
		require.NoError(t, json.Unmarshal(recorded, &record))
		require.Equal(t, namespace, record.Namespace)

		quota, err := os.ReadFile(filepath.Join(result.Path, "bootstrap", "overlays", "staging", resourceQuotaFile))
		require.NoError(t, err)
		var manifest struct {
			Metadata struct {
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		require.NoError(t, yaml.Unmarshal(quota, &manifest))
		require.Equal(t, namespace, manifest.Metadata.Namespace)

		bootstrap := t.TempDir()
		require.NoError(t, copyTree(result.Path, bootstrap))
		require.NoError(t, generateArgoBootstrap(
			ctx, &repositoryConfig{RepoURL: "https://git.example.com/acme/manifests.git"},
			bootstrap, "environments/deployments/modules/"+module, &result.Inventory, "staging", strings.Repeat("c", 40), "",
		))
		project, err := os.ReadFile(filepath.Join(bootstrap, "bootstrap", "project.yaml"))
		require.NoError(t, err)
		require.Contains(t, string(project), "namespace: "+namespace+"\n")
		set, err := os.ReadFile(filepath.Join(bootstrap, "bootstrap", "applicationset.yaml"))
		require.NoError(t, err)
		require.Contains(t, string(set), "namespace: "+namespace+"\n")
	}
}

// Every service tree a flow rendered is projected in its own module's namespace,
// and the environment-wide secret defaults substitute that module: two modules'
// "store" services bind to different namespaces and resolve different remote
// secrets from one declaration.
func TestProjectRenderedServiceConfigurationScopesEachModule(t *testing.T) {
	stage := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "acme",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "saas"}, {Name: "documents"}},
	}
	env := &environments.Environment{
		Name:      "staging",
		Namespace: "platform",
		ServiceSecrets: &environments.EnvironmentServiceSecrets{
			SecretStore: environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
			Defaults:    &environments.EnvironmentSecretRemoteRef{Key: "{workspace}-{module}-{service}", Property: "{key}"},
		},
	}
	graph := map[string]*resources.Service{}
	for _, module := range []string{"saas", "documents"} {
		root := filepath.Join(stage, "modules", module, serviceUnitDir, "store")
		writeServiceTreeReferencingKeys(t, root, env.Name, env.ModuleNamespace(workspace, module), "store", []string{"PASSWORD"})
		graph[resources.ServiceUnique(module, "store")] = &resources.Service{Name: "store"}
	}
	require.NoError(t, projectRenderedServiceConfiguration(t.Context(), stage, workspace, env, graph, nil))

	for module, namespace := range map[string]string{"saas": "platform-saas", "documents": "platform-documents"} {
		data, err := os.ReadFile(filepath.Join(stage, "modules", module, serviceUnitDir, "store", "overlays", "staging", "external-secret.yaml"))
		require.NoError(t, err)
		var projected externalSecret
		require.NoError(t, yaml.Unmarshal(data, &projected))
		require.Equal(t, namespace, projected.Metadata.Namespace)
		require.Len(t, projected.Spec.Data, 1)
		require.Equal(t, "acme-"+module+"-store", projected.Spec.Data[0].RemoteRef.Key)
		require.Equal(t, "PASSWORD", projected.Spec.Data[0].RemoteRef.Property)
	}
}

// A service with no per-service entry resolves through the environment-wide
// defaults; one with its own entry keeps its remote-keys and its own defaults
// ahead of the environment's; a service matched by nothing keeps "<service>/<key>".
func TestServiceSecretProjectionEnvironmentDefaults(t *testing.T) {
	secrets := &environments.EnvironmentServiceSecrets{
		SecretStore: environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
		Defaults:    &environments.EnvironmentSecretRemoteRef{Key: "{module}/{service}/{key}"},
		Services: map[string]environments.EnvironmentServiceSecretMapping{
			"accounts": {
				RemoteKeys: map[string]environments.EnvironmentSecretRemoteRef{"TOKEN": {Key: "shared/token"}},
				Defaults:   &environments.EnvironmentSecretRemoteRef{Key: "accounts-{key}"},
			},
		},
	}
	scope := unitScope{Workspace: "acme", Module: "documents", Namespace: "platform-documents"}

	store, err := serviceSecretProjection(scope, "store", secrets, []string{"PASSWORD"})
	require.NoError(t, err)
	require.Equal(t, "documents/store/PASSWORD", store.Spec.Data[0].RemoteRef.Key)
	require.Equal(t, "platform-documents", store.Metadata.Namespace)

	accounts, err := serviceSecretProjection(scope, "accounts", secrets, []string{"OTHER", "TOKEN"})
	require.NoError(t, err)
	require.Equal(t, "accounts-OTHER", accounts.Spec.Data[0].RemoteRef.Key)
	require.Equal(t, "shared/token", accounts.Spec.Data[1].RemoteRef.Key)

	secrets.Defaults = nil
	bare, err := serviceSecretProjection(scope, "store", secrets, []string{"PASSWORD"})
	require.NoError(t, err)
	require.Equal(t, "store/PASSWORD", bare.Spec.Data[0].RemoteRef.Key)
}

// The module bundle generator is handed the module's own namespace in every
// environment, so the bundle it emits — and the check holding it to that
// namespace — agree with the rest of the render.
func TestTransportNeutralModuleWorkspaceCarriesModuleNamespace(t *testing.T) {
	workspace := &resources.Workspace{
		Name:    "acme",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "saas"}, {Name: "documents"}},
		Environments: []*resources.Environment{resourceEnvironment(t, &environments.Environment{
			Name: "staging", Namespace: "platform", Cluster: &environments.EnvironmentCluster{Kind: "eks"},
		})},
	}
	for module, namespace := range map[string]string{"saas": "platform-saas", "documents": "platform-documents"} {
		encoded, err := encodeTransportNeutralModuleWorkspace(workspace, module)
		require.NoError(t, err)
		var decoded struct {
			Environments []struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"environments"`
		}
		require.NoError(t, yaml.Unmarshal(encoded, &decoded))
		require.Len(t, decoded.Environments, 1)
		require.Equal(t, namespace, decoded.Environments[0].Namespace, "module %s", module)
	}
}

// A solution may not take a module's derived namespace either.
func TestSolutionNamespaceRefusesModuleNamespaces(t *testing.T) {
	env := &environments.Environment{Name: "staging", Namespace: "platform"}
	workspace := &resources.Workspace{
		Name:    "acme",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "saas"}, {Name: "documents"}},
	}
	_, err := solutionNamespace("platform-saas", env, workspace)
	require.ErrorContains(t, err, "host namespace")
	_, err = solutionNamespace("platform", env, workspace)
	require.ErrorContains(t, err, "host namespace")
	namespace, err := solutionNamespace("lastlogin", env, workspace)
	require.NoError(t, err)
	require.Equal(t, "lastlogin", namespace)
}
