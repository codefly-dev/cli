package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const authorityPrefix = "CODEFLY__WORKSPACE_CONFIGURATION__MODULE_AUTHORITY__"
const authorityToken = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__MODULE_AUTHORITY__INTERNAL_TOKEN"

func TestModuleAuthorityMatchesDeliveredPresenceAndReusesInternalSecret(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	env.ServiceSecrets = storeEnvironment().ServiceSecrets
	// A known value exists in the composition, but rendering must never copy it.
	const secretValue = "module-authority-render-secret-canary"
	configurationDir := filepath.Join(workspace.Dir(), "configurations", env.Name)
	require.NoError(t, os.MkdirAll(configurationDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configurationDir, "internal-auth.secret.env"), []byte("CODEFLY_INTERNAL_TOKEN="+secretValue+"\n"), 0o600))
	require.NoError(t, refuseAuthoredRenderConfigurations(t.Context(), workspace, env.Runtime()))
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	principals := map[string]string{}
	// The same package composed under two module names gets different authority.
	for module, name := range map[string]string{"app": "api", "edge": "gateway"} {
		t.Run(module, func(t *testing.T) {
			service, err := workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: module, Name: name})
			require.NoError(t, err)
			service.WorkspaceConfigurationDependencies = append(service.WorkspaceConfigurationDependencies, environments.ModuleAuthorityConfiguration)
			scope := moduleScope(env, workspace, module)
			scope.WorkloadPeers = peers
			opts := solutionRenderOptions(filepath.Join(t.TempDir(), "rendered"))
			opts.Module, opts.Namespace, opts.Host = module, scope.Namespace, env.Host
			opts.Units = promotableServiceGraph(module, []string{name})
			instance := &opts.SolutionInstances[0]
			instance.Name, instance.Alias = module, module
			instance.Units = []SolutionArtifactUnit{{Name: name, Path: "services/" + name, Subject: "workload@example.test"}}
			instance.Endpoints = nil
			instance.Modules[0].Module = module
			result, err := RenderOwnedTree(t.Context(), opts, func(ctx context.Context, stage string) error {
				unit := filepath.Join(stage, "services", name)
				writeConsumerTree(t, unit, env.Name, scope.Namespace, name, "database.example:5432")
				// The existing internal-auth consumer uses exactly the same key.
				return projectServiceConfiguration(ctx, testRenderRoot(t, unit), unit, service, env, scope, serviceInjection{Secrets: map[string]string{storedSecretKey: storedSecretKey}})
			})
			require.NoError(t, err)
			presence := deliveredBinding(t, result.Path, "example.prod."+module)
			require.Len(t, presence.Workloads, 1)
			rendered := buildOverlay(t, filepath.Join(result.Path, "services", name), env.Name)
			data := configMapData(t, rendered)
			require.Equal(t, presence.Workloads[0].Identity.SPIFFEID, data[authorityPrefix+"PRINCIPAL"])
			// The host's name for a module capability's audience, the same for
			// every module, and never the module's own name.
			require.Equal(t, env.Host.ModuleAudience, data[authorityPrefix+"AUDIENCE"])
			require.Equal(t, "module-capabilities", data[authorityPrefix+"AUDIENCE"])
			require.Equal(t, module, presence.Modules[0].Module)
			require.Equal(t, presence.Workloads[0].Identity.Audience, data[authorityPrefix+"PROJECTION_AUDIENCE"])
			require.Equal(t, env.Host.Audience, data[authorityPrefix+"PROJECTION_AUDIENCE"])
			principals[module] = data[authorityPrefix+"PRINCIPAL"].(string)
			variables := containerEnvironment(t, rendered)
			require.Equal(t, variables[storedSecretKey]["valueFrom"], variables[authorityToken]["valueFrom"])
			require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "secret-" + name, "key": storedSecretKey, "optional": false}}, variables[authorityToken]["valueFrom"])
			require.NotContains(t, variables[authorityToken], "value")
			require.NotContains(t, data, authorityToken)
			secret := manifestOfKind(t, rendered, kindExternalSecret)
			entries := sliceField(mapField(secret.value, "spec"), "data")
			require.Len(t, entries, 1, "both carriers must use one delivered secret key")
			require.Equal(t, storedSecretKey, entries[0].(map[string]any)["secretKey"])
			require.NoError(t, walkRegularFiles(result.Path, func(path, _ string, _ os.FileInfo) error {
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NotContains(t, string(body), secretValue)
				return nil
			}))
		})
	}
	require.NotEqual(t, principals["app"], principals["edge"])
}

func TestModuleAuthorityRefusesMissingIdentityAudienceAndStore(t *testing.T) {
	for _, missing := range []string{"workload-identity", "host", "host.audience", "host.module_audience", "identity plan", "secret store"} {
		t.Run(missing, func(t *testing.T) {
			workspace, env := workloadIdentityComposition(t)
			env.ServiceSecrets = storeEnvironment().ServiceSecrets
			peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
			require.NoError(t, err)
			scope := moduleScope(env, workspace, "app")
			scope.WorkloadPeers = peers
			service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{"module-authority", "workload-identity"}}
			want := missing
			switch missing {
			case "workload-identity":
				service.WorkspaceConfigurationDependencies = []string{"module-authority"}
			case "host":
				env.Host = nil
				want = "host.audience"
			case "host.audience":
				env.Host.Audience = ""
			case "host.module_audience":
				env.Host.ModuleAudience = ""
			case "identity plan":
				scope.WorkloadPeers = nil
			case "secret store":
				env.ServiceSecrets = nil
			}
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, scope.Namespace, service.Name, "database.example:5432")
			err = projectServiceConfiguration(t.Context(), testRenderRoot(t, root), root, service, env, scope, serviceInjection{})
			require.ErrorContains(t, err, want)
		})
	}
}

func TestModuleAuthorityRequiresAnExplicitConsumerAndRefusesOverlayOverrides(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	env.ServiceSecrets = storeEnvironment().ServiceSecrets
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	scope := moduleScope(env, workspace, "app")
	scope.WorkloadPeers = peers
	for _, mode := range []string{"not requested", "principal override", "token override"} {
		t.Run(mode, func(t *testing.T) {
			service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{"workload-identity"}}
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
	for _, group := range []string{"workload-identity", "module-authority"} {
		for _, location := range []string{"configurations/prod", "modules/app/configurations/prod", "ambiguous", "empty"} {
			for _, extension := range []string{".env", ".secret.env"} {
				t.Run(group+"/"+location+extension, func(t *testing.T) {
					workspace, env := workloadIdentityComposition(t)
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
