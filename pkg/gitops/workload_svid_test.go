package gitops

import (
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

func workloadIdentityComposition(t *testing.T) (*resources.Workspace, *environments.Environment) {
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
      envelope_revision: 1
      delivery: edge/gateway/rest
      gateway: edge/gateway
`,
	}
	for module, service := range map[string]string{"edge": "gateway", "app": "api"} {
		files["modules/"+module+"/"+resources.ModuleConfigurationName] = "kind: module\nname: " + module + "\nservices:\n  - name: " + service + "\n"
		files["modules/"+module+"/services/"+service+"/"+resources.ServiceConfigurationName] = hostedServiceYAML(service, "workload-identity")
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

func TestWorkloadSVIDProjectsCompositionPeersAndCredentials(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	require.Equal(t, "spiffe://local.example/ns/example-edge/sa/gateway", peers["edge/gateway"].SPIFFEID)
	require.Equal(t, "spiffe://local.example/ns/example-app/sa/api", peers["app/api"].SPIFFEID)
	for module, name := range map[string]string{"edge": "gateway", "app": "api"} {
		t.Run(module, func(t *testing.T) {
			service, err := workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: module, Name: name})
			require.NoError(t, err)
			root := t.TempDir()
			body := strings.ReplaceAll(sidecarDeployment, "name: api", "name: "+name)
			body = strings.ReplaceAll(body, "serviceAccountName: api", "serviceAccountName: "+name)
			body = strings.Replace(body, "metadata:\n", "metadata:\n  namespace: "+env.ModuleNamespace(workspace, module)+"\n", 1)
			require.NoError(t, kustomizedWorkload(body)(t.Context(), root))
			unit := filepath.Join(root, "services", "api")
			scope := moduleScope(env, workspace, module)
			scope.WorkloadPeers = peers
			require.NoError(t, projectServiceConfiguration(t.Context(), testRenderRoot(t, unit), unit, service, env, scope, serviceInjection{}))
			_, err = validateTree(root, &RenderOptions{Module: module, Environment: env.Name, Namespace: scope.Namespace, Promotable: true, AppProject: "example"})
			require.NoError(t, err)
			docs, err := overlayManifests(filepath.Join(unit, "overlays", env.Name))
			require.NoError(t, err)
			seen := map[string]bool{}
			for _, doc := range docs {
				seen[doc.kind] = true
				switch doc.kind {
				case "Role":
					rules := sliceField(doc.value, "rules")
					require.Len(t, rules, 1)
					require.Equal(t, map[string]any{"apiGroups": []any{"cert-manager.io"}, "resources": []any{"certificaterequests"}, "verbs": []any{"create"}}, rules[0])
					require.Equal(t, scope.Namespace, metadataString(doc.value, "namespace"))
				case "RoleBinding":
					subject := sliceField(doc.value, "subjects")[0].(map[string]any)
					require.Equal(t, name, subject["name"])
					require.Equal(t, scope.Namespace, subject["namespace"])
					require.Equal(t, name+"-workload-identity", mapField(doc.value, "roleRef")["name"])
				case "ConfigMap":
					if metadataString(doc.value, "name") != name+"-workload-peers" {
						continue
					}
					want := peers["edge/gateway"].SPIFFEID + "\n"
					if module == "edge" {
						want = peers["app/api"].SPIFFEID + "\n" + want
					}
					data := mapField(doc.value, "data")
					require.Len(t, data, 3)
					for _, key := range []string{"allowed-callers", "mint-peers", "gateway-peers"} {
						require.Equal(t, want, data[key])
					}
				case "Deployment":
					spec, _ := podSpec(doc)
					require.Equal(t, name, spec["serviceAccountName"])
					volumes := map[string]map[string]any{}
					for _, raw := range sliceField(spec, "volumes") {
						v := raw.(map[string]any)
						volumes[v["name"].(string)] = v
					}
					require.Equal(t, map[string]any{"driver": "spiffe.csi.cert-manager.io", "readOnly": true}, mapField(volumes[svidVolume], "csi"))
					require.Equal(t, name+"-workload-peers", mapField(volumes[peersVolume], "configMap")["name"])
					require.NotNil(t, volumes[identityTokenVolume])
					for _, raw := range sliceField(spec, "containers") {
						c := raw.(map[string]any)
						if c["name"] == "proxy" {
							require.Empty(t, sliceField(c, "volumeMounts"))
							require.Empty(t, sliceField(c, "env"))
							continue
						}
						variables := map[string]any{}
						for _, raw := range sliceField(c, "env") {
							entry := raw.(map[string]any)
							variables[entry["name"].(string)] = entry["value"]
							require.Nil(t, entry["valueFrom"])
						}
						for key, value := range workloadIdentityPaths() {
							require.Equal(t, value, variables["CODEFLY__WORKSPACE_CONFIGURATION__WORKLOAD_IDENTITY__"+key])
						}
						mounts := map[string]string{}
						for _, raw := range sliceField(c, "volumeMounts") {
							mount := raw.(map[string]any)
							require.Equal(t, true, mount["readOnly"])
							mounts[mount["name"].(string)] = mount["mountPath"].(string)
							require.NotContains(t, mount, "subPath")
						}
						require.Equal(t, svidMount, mounts[svidVolume])
						require.Equal(t, peersMount, mounts[peersVolume])
						require.Equal(t, identityTokenMount, mounts[identityTokenVolume])
					}
				}
			}
			for _, kind := range []string{"Role", "RoleBinding", "ConfigMap", "Deployment", "ServiceAccount"} {
				require.True(t, seen[kind], kind)
			}
		})
	}
	require.Equal(t, map[string]string{
		"TOKEN_FILE":           "/var/run/secrets/codefly/token",
		"CERT_FILE":            "/var/run/secrets/spiffe.io/tls.crt",
		"KEY_FILE":             "/var/run/secrets/spiffe.io/tls.key",
		"TRUST_BUNDLE_FILE":    "/var/run/secrets/spiffe.io/ca.crt",
		"ALLOWED_CALLERS_FILE": "/etc/codefly/workload-peers/allowed-callers",
		"MINT_PEERS_FILE":      "/etc/codefly/workload-peers/mint-peers",
		"GATEWAY_PEERS_FILE":   "/etc/codefly/workload-peers/gateway-peers",
	}, workloadIdentityPaths())
}

func TestWorkloadSVIDRefusesConflictsAndDoesNotProjectNonConsumers(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	scope := moduleScope(env, workspace, "app")
	scope.WorkloadPeers = peers
	root := t.TempDir()
	require.NoError(t, projectWorkloadSVID(testRenderRoot(t, root), root, &resources.Service{Name: "plain"}, env, scope))
	service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{"workload-identity"}}
	for _, tc := range []struct{ name, body, want string }{
		{"account", strings.ReplaceAll(pinnedDeployment, "serviceAccountName: api", "serviceAccountName: other"), "not the resolved"},
		{"shadow", pinnedDeployment + "          volumeMounts:\n            - name: other\n              mountPath: /var/run/secrets\n", "shadows"},
		{"reserved", strings.Replace(pinnedDeployment, "      containers:", "      volumes:\n        - name: codefly-workload-svid\n          emptyDir: {}\n      containers:", 1), "reserved volume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, kustomizedWorkload(tc.body)(t.Context(), root))
			err := projectWorkloadSVID(testRenderRoot(t, root), filepath.Join(root, "services", "api"), service, env, scope)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestWorkloadSVIDRefusesHostileRenderNamesBeforeWriting(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	for _, field := range []string{"module", "service", "environment"} {
		invalid := []string{"", ".", "..", "../escape", "nested/api", `nested\api`, "/absolute", "api..escape", "api\x00"}
		if field != "environment" {
			invalid = append(invalid, "Uppercase", "api_service", "api.service", "api--service")
		}
		for _, name := range invalid {
			t.Run(field+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				require.NoError(t, kustomizedWorkload(pinnedDeployment)(t.Context(), root))
				before := snapshotTree(t, root)
				unit := filepath.Join(root, "services", "api")
				scope := moduleScope(env, workspace, "app")
				scope.WorkloadPeers = peers
				service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{environments.WorkloadIdentityConfiguration}}
				target := *env
				switch field {
				case "module":
					scope.Module = name
				case "service":
					service.Name = name
				case "environment":
					target.Name = name
				}
				require.ErrorContains(t, projectWorkloadSVID(testRenderRoot(t, root), unit, service, &target, scope), fmt.Sprintf("%s %q", field, name))
				require.ErrorContains(t, validateProjectedWorkloadSVID(testRenderRoot(t, root), unit, service, &target, scope), fmt.Sprintf("%s %q", field, name))
				// The outer projection must refuse the name before earlier steps
				// can use it to write a ServiceAccount or configuration resource.
				require.ErrorContains(t, projectServiceConfiguration(t.Context(), testRenderRoot(t, unit), unit, service, &target, scope, serviceInjection{}), fmt.Sprintf("%s %q", field, name))
				require.Equal(t, before, snapshotTree(t, root))
			})
		}
	}
}

func TestWorkloadSVIDRefusesOverlayOutsideRenderRoot(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	scope := moduleScope(env, workspace, "app")
	scope.WorkloadPeers = peers
	service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{environments.WorkloadIdentityConfiguration}}
	root := t.TempDir()
	external := t.TempDir()
	require.NoError(t, kustomizedWorkload(pinnedDeployment)(t.Context(), root))
	unit := filepath.Join(root, "services", "api")
	overlay := filepath.Join(unit, "overlays", env.Name)
	require.NoError(t, os.Rename(overlay, filepath.Join(external, env.Name)))
	require.NoError(t, os.Symlink(filepath.Join(external, env.Name), overlay))
	before := snapshotTree(t, external)
	require.ErrorContains(t, projectWorkloadSVID(testRenderRoot(t, root), unit, service, env, scope), "symbolic link")
	require.ErrorContains(t, validateProjectedWorkloadSVID(testRenderRoot(t, root), unit, service, env, scope), "symbolic link")
	require.Equal(t, before, snapshotTree(t, external))
}

func TestWorkloadSVIDPlainOverlayReplacesAuthoredPathSources(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	root := t.TempDir()
	body := pinnedDeployment + `          env:
            - name: CODEFLY__WORKSPACE_CONFIGURATION__WORKLOAD_IDENTITY__TOKEN_FILE
              valueFrom:
                configMapKeyRef:
                  name: obsolete-path
                  key: path
            - name: KEEP_ME
              value: present
`
	require.NoError(t, renderWorkload(body)(t.Context(), root))
	scope := moduleScope(env, workspace, "app")
	scope.WorkloadPeers = peers
	service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{"workload-identity"}}
	unit := filepath.Join(root, "services", "api")
	require.NoError(t, projectWorkloadSVID(testRenderRoot(t, root), unit, service, env, scope))
	docs, err := overlayManifests(filepath.Join(unit, "overlays", env.Name))
	require.NoError(t, err)
	for _, doc := range docs {
		if doc.kind != "Deployment" {
			continue
		}
		spec, _ := podSpec(doc)
		container := sliceField(spec, "containers")[0].(map[string]any)
		variables := map[string]any{}
		for _, raw := range sliceField(container, "env") {
			entry := raw.(map[string]any)
			variables[entry["name"].(string)] = entry["value"]
			require.Nil(t, entry["valueFrom"])
		}
		require.Equal(t, "present", variables["KEEP_ME"])
		require.Equal(t, "/var/run/secrets/codefly/token", variables["CODEFLY__WORKSPACE_CONFIGURATION__WORKLOAD_IDENTITY__TOKEN_FILE"])
		require.Len(t, variables, 8)
	}
}

func TestWorkloadIdentityPathsPassManifestGuardWithoutAdmittingCredentials(t *testing.T) {
	const key = "CODEFLY__WORKSPACE_CONFIGURATION__WORKLOAD_IDENTITY__TOKEN_FILE"
	for _, shape := range configurationShapes(key) {
		if data := mapField(shape, "data"); data != nil {
			data[key] = "/var/run/secrets/codefly/token"
		}
		if data := mapField(shape, "stringData"); data != nil {
			data[key] = "/var/run/secrets/codefly/token"
		}
		for _, raw := range sliceField(shape, "env") {
			raw.(map[string]any)["value"] = "/var/run/secrets/codefly/token"
		}
		require.NoError(t, inspectValue(shape, nil, true))
	}
	for _, value := range []string{"a-bearer-token", "/some/other/path", "/var/run/secrets/codefly/token\n"} {
		require.ErrorContains(t, inspectValue(map[string]any{"env": []any{map[string]any{"name": key, "value": value}}}, nil, true), "credential value")
	}
	require.Error(t, inspectValue(map[string]any{"env": []any{map[string]any{"name": "ACCESS_TOKEN_FILE", "value": "/var/run/secrets/codefly/token"}}}, nil, true))
}

func TestWorkloadSVIDRefusesAuthoredPeerPatches(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	for module, name := range map[string]string{"app": "api", "edge": "gateway"} {
		for _, key := range []string{"allowed-callers", "mint-peers", "gateway-peers"} {
			t.Run(module+"/"+key, func(t *testing.T) {
				service, err := workspace.LoadService(t.Context(), &resources.ServiceWithModule{Module: module, Name: name})
				require.NoError(t, err)
				root := t.TempDir()
				body := strings.ReplaceAll(sidecarDeployment, "name: api", "name: "+name)
				body = strings.ReplaceAll(body, "serviceAccountName: api", "serviceAccountName: "+name)
				scope := moduleScope(env, workspace, module)
				scope.WorkloadPeers = peers
				body = strings.Replace(body, "metadata:\n", "metadata:\n  namespace: "+scope.Namespace+"\n", 1)
				require.NoError(t, kustomizedWorkload(body)(t.Context(), root))
				unit := filepath.Join(root, "services", "api")
				appendIdentityPatch(t, unit, env.Name, kindConfigMap, name+"-workload-peers", "- op: add\n  path: /data/"+key+"\n  value: spiffe://local.example/ns/example-app/sa/non-declaring\n")
				err = projectServiceConfiguration(t.Context(), testRenderRoot(t, unit), unit, service, env, scope, serviceInjection{})
				require.ErrorContains(t, err, "ConfigMap "+scope.Namespace+"/"+name+"-workload-peers")
				require.ErrorContains(t, err, "composition workload peer plan")
			})
		}
	}
}

func appendIdentityPatch(t *testing.T, unit, environment, kind, name, patch string) {
	t.Helper()
	file := filepath.Join(unit, "overlays", environment, kustomizationFile)
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var customization map[string]any
	require.NoError(t, yaml.Unmarshal(data, &customization))
	patches, _ := customization["patches"].([]any)
	customization["patches"] = append(patches, map[string]any{"target": map[string]any{"kind": kind, "name": name}, "patch": patch})
	data, err = yaml.Marshal(customization)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0o600))
}

func TestWorkloadSVIDValidatesEffectiveBindings(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	for _, tc := range []struct{ name, path, value, want string }{
		{"peer source", "/spec/template/spec/volumes/0/configMap", "{name: other}", "volumes"},
		{"peer items", "/spec/template/spec/volumes/0/configMap/items", "[{key: allowed-callers, path: gateway-peers}]", "volumes"},
		{"CSI driver", "/spec/template/spec/volumes/1/csi/driver", "other.csi", "volumes"},
		{"subPath", "/spec/template/spec/containers/0/volumeMounts/0/subPath", "other", "mount"},
		{"mount source", "/spec/template/spec/containers/0/volumeMounts/0/name", "other", "mount"},
		{"mount path", "/spec/template/spec/containers/0/volumeMounts/0/mountPath", "/other", "mount"},
		{"writable", "/spec/template/spec/containers/0/volumeMounts/0/readOnly", "false", "mount"},
		{"env path", "/spec/template/spec/containers/0/env/0/value", "/other", "env"},
		{"env source", "/spec/template/spec/containers/0/env/0/valueFrom", "{configMapKeyRef: {name: other, key: path}}", "env"},
		{"sidecar mount", "/spec/template/spec/containers/1/volumeMounts", "[{name: codefly-workload-svid, mountPath: /stolen}]", "mount"},
		{"shadow", "/spec/template/spec/containers/0/volumeMounts/-", "{name: other, mountPath: /etc/codefly}", "mount"},
		{"account", "/spec/template/spec/serviceAccountName", "other", "ServiceAccount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			scope := moduleScope(env, workspace, "app")
			scope.WorkloadPeers = peers
			body := strings.Replace(sidecarDeployment, "metadata:\n", "metadata:\n  namespace: "+scope.Namespace+"\n", 1)
			require.NoError(t, kustomizedWorkload(body)(t.Context(), root))
			unit := filepath.Join(root, "services", "api")
			service := &resources.Service{Name: "api", WorkspaceConfigurationDependencies: []string{"workload-identity"}}
			require.NoError(t, projectServiceConfiguration(t.Context(), testRenderRoot(t, unit), unit, service, env, scope, serviceInjection{}))
			// Determine indexes from effective output; Kustomize can reorder lists.
			docs, err := overlayManifests(filepath.Join(unit, "overlays", env.Name))
			require.NoError(t, err)
			patchPath := tc.path
			for _, doc := range docs {
				if doc.kind != kindDeployment {
					continue
				}
				spec, _ := podSpec(doc)
				if strings.Contains(patchPath, "/volumes/") {
					wanted := peersVolume
					if tc.name == "CSI driver" {
						wanted = svidVolume
					}
					for i, raw := range sliceField(spec, "volumes") {
						if raw.(map[string]any)["name"] == wanted {
							parts := strings.Split(patchPath, "/")
							parts[5] = fmt.Sprint(i)
							patchPath = strings.Join(parts, "/")
						}
					}
				}
			}
			appendIdentityPatch(t, unit, env.Name, kindDeployment, "api", "- op: add\n  path: "+patchPath+"\n  value: "+tc.value+"\n")
			err = validateProjectedWorkloadSVID(testRenderRoot(t, root), unit, service, env, scope)
			require.ErrorContains(t, err, "api")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestWorkloadSVIDRechecksPeersBeforeTreePromotion(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	for module, name := range map[string]string{"app": "api", "edge": "gateway"} {
		for _, key := range []string{"allowed-callers", "mint-peers", "gateway-peers"} {
			t.Run(module+"/"+key, func(t *testing.T) {
				scope := moduleScope(env, workspace, module)
				scope.WorkloadPeers = peers
				service := &resources.Service{Name: name, WorkspaceConfigurationDependencies: []string{"workload-identity"}}
				root := t.TempDir()
				body := strings.ReplaceAll(sidecarDeployment, "name: api", "name: "+name)
				body = strings.ReplaceAll(body, "serviceAccountName: api", "serviceAccountName: "+name)
				body = strings.Replace(body, "metadata:\n", "metadata:\n  namespace: "+scope.Namespace+"\n", 1)
				require.NoError(t, kustomizedWorkload(body)(t.Context(), root))
				unit := filepath.Join(root, "services", "api")
				require.NoError(t, projectServiceConfiguration(t.Context(), testRenderRoot(t, unit), unit, service, env, scope, serviceInjection{}))
				options := &RenderOptions{
					Module: module, Environment: env.Name, Namespace: scope.Namespace, Promotable: true,
					UnitNames:              []string{"api"},
					workloadIdentityChecks: []workloadIdentityCheck{{path: "services/api", service: service, environment: env, scope: scope}},
				}
				captureIdentityChecks(t, root, options)
				_, err := validateTree(root, options)
				require.NoError(t, err)
				appendIdentityPatch(t, unit, env.Name, kindConfigMap, name+"-workload-peers", "- op: add\n  path: /data/"+key+"\n  value: spiffe://local.example/ns/example-app/sa/non-declaring\n")
				_, err = validateTree(root, options)
				require.ErrorContains(t, err, "ConfigMap "+scope.Namespace+"/"+name+"-workload-peers")
			})
		}
	}
}

func TestWorkloadSVIDRefusesSiblingPeerConfigMaps(t *testing.T) {
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	for module, name := range map[string]string{"app": "api", "edge": "gateway"} {
		for _, variant := range []string{"changed data", "identical data", "different namespace", "wrong owner"} {
			t.Run(module+"/"+variant, func(t *testing.T) {
				scope := moduleScope(env, workspace, module)
				scope.WorkloadPeers = peers
				service := &resources.Service{Name: name, WorkspaceConfigurationDependencies: []string{"workload-identity"}}
				sibling := &resources.Service{Name: "sibling"}
				root := t.TempDir()
				for _, svc := range []*resources.Service{service, sibling} {
					staging := t.TempDir()
					body := strings.ReplaceAll(sidecarDeployment, "name: api", "name: "+svc.Name)
					body = strings.ReplaceAll(body, "serviceAccountName: api", "serviceAccountName: "+svc.Name)
					body = strings.Replace(body, "metadata:\n", "metadata:\n  namespace: "+scope.Namespace+"\n", 1)
					require.NoError(t, kustomizedWorkload(body)(t.Context(), staging))
					require.NoError(t, os.MkdirAll(filepath.Join(root, "services"), 0o755))
					unit := filepath.Join(root, "services", svc.Name)
					require.NoError(t, os.Rename(filepath.Join(staging, "services", "api"), unit))
					require.NoError(t, projectServiceConfiguration(t.Context(), testRenderRoot(t, unit), unit, svc, env, scope, serviceInjection{}))
				}
				options := &RenderOptions{
					Module: module, Environment: env.Name, Namespace: scope.Namespace, Promotable: true,
					CheckUnitDirectories: true,
					Units: []InventoryUnit{
						{Name: name, Kind: "service", Path: "services/" + name},
						{Name: sibling.Name, Kind: "service", Path: "services/" + sibling.Name},
					},
					workloadIdentityChecks: []workloadIdentityCheck{
						{path: "services/" + name, service: service, environment: env, scope: scope},
						{path: "services/" + sibling.Name, service: sibling, environment: env, scope: scope},
					},
				}
				captureIdentityChecks(t, root, options)
				_, err := validateTree(root, options)
				require.NoError(t, err)
				files, err := peers.files(env.Host, resources.ServiceUnique(module, name))
				require.NoError(t, err)
				if variant == "changed data" {
					for key := range files {
						files[key] = "spiffe://local.example/ns/example-app/sa/non-declaring\n"
					}
				}
				namespace := scope.Namespace
				if variant == "different namespace" {
					namespace += "-other"
				}
				collision := map[string]any{
					keyAPIVersion: "v1", keyKind: kindConfigMap,
					keyMetadata: map[string]any{keyName: name + "-workload-peers", keyNamespace: namespace},
					dataKey:     files,
				}
				data, err := yaml.Marshal(collision)
				require.NoError(t, err)
				overlay := filepath.Join(root, "services", sibling.Name, "overlays", env.Name)
				require.NoError(t, os.WriteFile(filepath.Join(overlay, "collision.yaml"), data, 0o600))
				require.NoError(t, addKustomizationResource(overlay, "collision.yaml"))
				if variant == "wrong owner" {
					appendIdentityPatch(t, filepath.Join(root, "services", name), env.Name, kindConfigMap, name+"-workload-peers", "- op: replace\n  path: /metadata/name\n  value: renamed-peers\n")
				}
				_, err = validateTree(root, options)
				if variant == "different namespace" {
					require.NoError(t, err)
					return
				}
				require.ErrorContains(t, err, "ConfigMap "+scope.Namespace+"/"+name+"-workload-peers")
				require.ErrorContains(t, err, "services/"+name+"/")
				require.ErrorContains(t, err, "services/sibling/overlays/"+env.Name+"/rendered.yaml")
			})
		}
	}
}

func captureIdentityChecks(t *testing.T, root string, options *RenderOptions) {
	t.Helper()
	tree := testRenderRoot(t, root)
	for i := range options.workloadIdentityChecks {
		require.NoError(t, options.workloadIdentityChecks[i].capture(tree, root))
	}
}
