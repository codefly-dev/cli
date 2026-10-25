package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

const authorityPrefix = "CODEFLY__WORKSPACE_CONFIGURATION__MODULE_AUTHORITY__"
const authorityToken = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__MODULE_AUTHORITY__INTERNAL_TOKEN"

// hostComposition composes two modules, edge (the host's gateway) and app,
// under an environment that declares a host: the composition every render
// that projects a mesh principal or module authority is tested against.
func hostComposition(t *testing.T) (*resources.Workspace, *environments.Environment) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: example
layout: modules
modules:
  - name: edge
  - name: app
environments:
  - name: prod
    namespace: example
    cluster:
      kind: k3d
      context: k3d-example
    host:
      coordinate: example/local/dev
      component: host
      domain: example
      audience: mint
      module_audience: module-capabilities
      trust_domain: local.example
      delivery: edge/gateway/rest
      gateway: edge/gateway
`,
	}
	for module, service := range map[string]string{"edge": "gateway", "app": "api"} {
		files["modules/"+module+"/"+resources.ModuleConfigurationName] = "kind: module\nname: " + module + "\nservices:\n  - name: " + service + "\n"
		files["modules/"+module+"/services/"+service+"/"+resources.ServiceConfigurationName] = hostedServiceYAML(service)
	}
	for name, content := range files {
		file := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	env, err := environments.Select(workspace, "prod")
	require.NoError(t, err)
	return workspace, env
}

// The principal module-authority hands a service is its mesh principal: the
// SPIFFE ID the cell's mesh issues to the ServiceAccount the rendered pod
// runs as, the same one its module manifest names. Nothing is issued or mounted for
// it, and the internal token aliases the existing internal-auth secret.
func TestModuleAuthorityMatchesTheDeliveredManifestAndReusesInternalSecret(t *testing.T) {
	workspace, env := hostComposition(t)
	env.ServiceSecrets = storeEnvironment().ServiceSecrets
	// A known value exists in the composition, but rendering must never copy it.
	const secretValue = "module-authority-render-secret-canary"
	configurationDir := filepath.Join(workspace.Dir(), "configurations", env.Name)
	require.NoError(t, os.MkdirAll(configurationDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configurationDir, "internal-auth.secret.env"), []byte("CODEFLY_INTERNAL_TOKEN="+secretValue+"\n"), 0o600))
	require.NoError(t, refuseAuthoredRenderConfigurations(t.Context(), workspace, env.Runtime()))
	principals := map[string]string{}
	// The same package composed under two module names gets different authority.
	for module, name := range map[string]string{"app": "api", "edge": "gateway"} {
		t.Run(module, func(t *testing.T) {
			service, err := workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: module, Name: name})
			require.NoError(t, err)
			service.WorkspaceConfigurationDependencies = append(service.WorkspaceConfigurationDependencies, environments.ModuleAuthorityConfiguration)
			scope := moduleScope(env, workspace, module)
			opts := solutionRenderOptions(filepath.Join(t.TempDir(), "rendered"))
			opts.Module, opts.Namespace, opts.Host = module, scope.Namespace, env.Host
			opts.Units = promotableServiceGraph(module, []string{name})
			instance := opts.Instance
			instance.Name = module
			instance.Identities = []IdentityWorkload{{Service: name, Path: "services/" + name}}
			instance.Endpoints = nil
			instance.Authority = &solutionhost.Authority{Principal: module}
			result, err := RenderOwnedTree(t.Context(), opts, func(ctx context.Context, stage string) error {
				unit := filepath.Join(stage, "services", name)
				writeConsumerTree(t, unit, env.Name, scope.Namespace, name, "database.example:5432")
				// The existing internal-auth consumer uses exactly the same key.
				return projectServiceConfiguration(ctx, testRenderRoot(t, unit), unit, service, env, scope, serviceInjection{Secrets: map[string]string{storedSecretKey: storedSecretKey}})
			})
			require.NoError(t, err)
			manifest := deliveredModuleManifest(t, result.Path, "example.prod."+module)
			require.Len(t, manifest.Workloads, 1)
			rendered := buildOverlay(t, filepath.Join(result.Path, "services", name), env.Name)
			data := configMapData(t, rendered)
			// A cell's principal is the mesh identity of the declared workload:
			// its module's delivered namespace (<delivery>-<environment>-<module>,
			// handbook decisions/a-delivered-namespace-is-named-by-its-delivery-environment-and-module.md),
			// as the account named after the service.
			require.Equal(t, "spiffe://local.example/ns/example-"+module+"/sa/"+name, data[authorityPrefix+"PRINCIPAL"])
			// The host's name for a module capability's audience, the same for
			// every module, and never the module's own name.
			require.Equal(t, env.Host.ModuleAudience, data[authorityPrefix+"AUDIENCE"])
			require.Equal(t, "module-capabilities", data[authorityPrefix+"AUDIENCE"])
			require.Equal(t, module, manifest.Authority.Principal)
			require.Equal(t, env.Host.Audience, data[authorityPrefix+"PROJECTION_AUDIENCE"])
			principals[module] = data[authorityPrefix+"PRINCIPAL"].(string)
			variables := containerEnvironment(t, rendered)
			require.Equal(t, variables[storedSecretKey]["valueFrom"], variables[authorityToken]["valueFrom"])
			require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "secret-" + name, "key": storedSecretKey, "optional": false}}, variables[authorityToken]["valueFrom"])
			require.NotContains(t, variables[authorityToken], "value")
			require.NotContains(t, data, authorityToken)
			for key := range variables {
				require.NotContains(t, key, "WORKLOAD_IDENTITY", "no SVID, trust bundle or peer file is handed to a workload")
			}
			secret := manifestOfKind(t, rendered, kindExternalSecret)
			entries := sliceField(mapField(secret.value, "spec"), "data")
			require.Len(t, entries, 1, "both carriers must use one delivered secret key")
			require.Equal(t, storedSecretKey, entries[0].(map[string]any)["secretKey"])
			require.NoError(t, walkRegularFiles(result.Path, func(path, _ string, _ os.FileInfo) error {
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NotContains(t, string(body), secretValue)
				require.NotContains(t, string(body), "spiffe.csi.cert-manager.io")
				require.NotContains(t, string(body), "workload-peers")
				return nil
			}))
		})
	}
	require.NotEqual(t, principals["app"], principals["edge"])
}

func TestModuleAuthorityRefusesMissingAudienceNamespaceAndStore(t *testing.T) {
	for _, missing := range []string{"host", "host.audience", "host.module_audience", "namespace", "secret store"} {
		t.Run(missing, func(t *testing.T) {
			workspace, env := hostComposition(t)
			env.ServiceSecrets = storeEnvironment().ServiceSecrets
			scope := moduleScope(env, workspace, "app")
			service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{"module-authority"}}
			want := missing
			switch missing {
			case "host":
				env.Host = nil
				want = "host.audience"
			case "host.audience":
				env.Host.Audience = ""
			case "host.module_audience":
				env.Host.ModuleAudience = ""
			case "namespace":
				scope.Namespace = ""
				want = "mesh principal"
			case "secret store":
				env.ServiceSecrets = nil
			}
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, scope.Namespace, service.Name, "database.example:5432")
			err := projectServiceConfiguration(t.Context(), testRenderRoot(t, root), root, service, env, scope, serviceInjection{})
			require.ErrorContains(t, err, want)
		})
	}
}

func TestModuleAuthorityRequiresAnExplicitConsumerAndRefusesOverlayOverrides(t *testing.T) {
	workspace, env := hostComposition(t)
	env.ServiceSecrets = storeEnvironment().ServiceSecrets
	scope := moduleScope(env, workspace, "app")
	for _, mode := range []string{"not requested", "principal override", "token override"} {
		t.Run(mode, func(t *testing.T) {
			service := &resources.Service{Name: "api"}
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, scope.Namespace, service.Name, "database.example:5432")
			if mode != "not requested" {
				service.WorkspaceConfigurationDependencies = append(service.WorkspaceConfigurationDependencies, "module-authority")
				if mode == "principal override" {
					addProjectionPatch(t, root, env.Name, "ConfigMap", "- op: add\n  path: /data/"+authorityPrefix+"PRINCIPAL\n  value: wrong\n")
				} else {
					addProjectionPatch(t, root, env.Name, kindDeployment, "- op: replace\n  path: /spec/template/spec/containers/0/env/0/valueFrom/secretKeyRef/key\n  value: WRONG\n")
				}
			}
			err := projectServiceConfiguration(t.Context(), testRenderRoot(t, root), root, service, env, scope, serviceInjection{})
			if mode != "not requested" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			rendered := buildOverlay(t, root, env.Name)
			require.NotContains(t, configMapData(t, rendered), authorityPrefix+"PRINCIPAL")
			require.NotContains(t, containerEnvironment(t, rendered), authorityToken)
		})
	}
}

func TestRenderRefusesAuthoredCompositionGroups(t *testing.T) {
	// egress is generated from the environment's egress declaration: an
	// authored copy of it is the hand-kept list it replaces.
	for _, group := range []string{"module-authority", environments.EgressConfiguration} {
		for _, location := range []string{"configurations/prod", "modules/app/configurations/prod", "ambiguous", "empty"} {
			for _, extension := range []string{".env", ".secret.env"} {
				t.Run(group+"/"+location+extension, func(t *testing.T) {
					workspace, env := hostComposition(t)
					// A composed module's own workspace is its configuration boundary.
					for name, service := range map[string]string{"app": "api", "edge": "gateway"} {
						path := filepath.Join(workspace.Dir(), "modules", name, resources.WorkspaceConfigurationName)
						require.NoError(t, os.WriteFile(path, []byte("name: "+name+"\nlayout: flat\n"), 0o600))
						// Even an unconsumed file is refused, before any build starts.
						path = filepath.Join(workspace.Dir(), "modules", name, "services", service, resources.ServiceConfigurationName)
						require.NoError(t, os.WriteFile(path, []byte(hostedServiceYAML(service)), 0o600))
					}
					var err error
					workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
					require.NoError(t, err)
					locations := []string{location}
					if location == "ambiguous" {
						locations = []string{"modules/app/configurations/prod", "modules/edge/configurations/prod"}
					} else if location == "empty" {
						locations = []string{"configurations/prod"}
					}
					for _, directory := range locations {
						path := filepath.Join(workspace.Dir(), directory, group+extension)
						require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
						body := "PRINCIPAL=" + directory + "\n"
						if location == "empty" {
							body = ""
						}
						require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
					}
					module, err := workspace.LoadModuleFromName(t.Context(), "app")
					require.NoError(t, err)
					_, err = RenderModule(t.Context(), workspace, module, env, "example", nil)
					require.ErrorContains(t, err, "owned by the render")
					require.False(t, strings.Contains(err.Error(), "registry"), "refuse before builds")
					service, err := module.LoadServiceFromName(t.Context(), "api")
					require.NoError(t, err)
					_, err = RenderService(t.Context(), workspace, module, service, env, "example", false, nil)
					require.ErrorContains(t, err, "owned by the render")
				})
			}
		}
	}
}
