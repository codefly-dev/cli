package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func identityTreeFixture(t *testing.T) (string, *RenderOptions) {
	t.Helper()
	workspace, env := workloadIdentityComposition(t)
	peers, err := resolveWorkloadPeers(t.Context(), workspace, env)
	require.NoError(t, err)
	scope := moduleScope(env, workspace, "app")
	scope.WorkloadPeers = peers
	root := t.TempDir()
	options := &RenderOptions{Module: "app", Environment: env.Name, Namespace: scope.Namespace, Promotable: true, CheckUnitDirectories: true}
	tree := testRenderRoot(t, root)
	for _, name := range []string{"api", "sibling"} {
		stage := t.TempDir()
		body := strings.ReplaceAll(sidecarDeployment, "name: api", "name: "+name)
		body = strings.ReplaceAll(body, "serviceAccountName: api", "serviceAccountName: "+name)
		body = strings.Replace(body, "metadata:\n", "metadata:\n  namespace: "+scope.Namespace+"\n", 1)
		require.NoError(t, kustomizedWorkload(body)(t.Context(), stage))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "services"), 0o755))
		unit := filepath.Join(root, "services", name)
		require.NoError(t, os.Rename(filepath.Join(stage, "services", "api"), unit))
		service := &resources.Service{Name: name}
		if name == "api" {
			service.WorkspaceConfigurationDependencies = []string{"workload-identity"}
		}
		require.NoError(t, projectServiceConfiguration(t.Context(), tree, unit, service, env, scope, serviceInjection{}))
		options.Units = append(options.Units, InventoryUnit{Module: "app", Name: name, Kind: UnitKindService, Path: "services/" + name, Output: restrictedRenderEvidence()})
		check := workloadIdentityCheck{path: "services/" + name, service: service, environment: env, scope: scope}
		require.NoError(t, check.capture(tree, root))
		options.workloadIdentityChecks = append(options.workloadIdentityChecks, check)
	}
	_, err = validateTree(root, options)
	require.NoError(t, err)
	return root, options
}

func assertIdentityTreeRefused(t *testing.T, root string, options *RenderOptions, paths ...string) {
	t.Helper()
	_, err := validateTree(root, options)
	require.Error(t, err)
	for _, path := range paths {
		require.ErrorContains(t, err, path)
	}
	options.Destination = filepath.Join(t.TempDir(), "promoted")
	_, err = RenderOwnedTree(t.Context(), options, func(_ context.Context, stage string) error { return copyTree(root, stage) })
	require.Error(t, err)
	for _, path := range paths {
		require.ErrorContains(t, err, path)
	}
	_, err = os.Stat(options.Destination)
	require.True(t, os.IsNotExist(err))
}

func TestPromotedTreeRefusesDefaultedPeerCollision(t *testing.T) {
	for _, owner := range []string{"api", "sibling"} {
		for _, spelling := range []string{"absent", "empty"} {
			t.Run(owner+"/"+spelling, func(t *testing.T) {
				root, options := identityTreeFixture(t)
				metadata := map[string]any{keyName: "api-workload-peers"}
				if spelling == "empty" {
					metadata[keyNamespace] = ""
				}
				overlay := filepath.Join(root, "services", owner, "overlays", options.Environment)
				require.NoError(t, writeOverlayResource(overlay, "collision.yaml", map[string]any{
					keyAPIVersion: "v1", keyKind: kindConfigMap, keyMetadata: metadata,
					dataKey: map[string]any{"gateway-peers": "spiffe://attacker\n"},
				}))
				assertIdentityTreeRefused(t, root, options, "ConfigMap "+options.Namespace+"/api-workload-peers", "services/api/", "services/"+owner+"/")
			})
		}
	}
}

func TestPromotedTreeRefusesSiblingOwnedObjects(t *testing.T) {
	for _, variant := range []string{"identical workload", "volume source", "mount path", "GATEWAY_PEERS_FILE", "ServiceAccount", "Role", "RoleBinding"} {
		t.Run(variant, func(t *testing.T) {
			root, options := identityTreeFixture(t)
			kind := kindDeployment
			if variant == "ServiceAccount" || variant == "Role" || variant == "RoleBinding" {
				kind = variant
			}
			var object map[string]any
			for _, doc := range options.workloadIdentityChecks[0].objects {
				if doc.kind == kind {
					// Copy the retained expectation rather than mutate it.
					encoded, err := yaml.Marshal(doc.value)
					require.NoError(t, err)
					require.NoError(t, yaml.Unmarshal(encoded, &object))
					break
				}
			}
			require.NotNil(t, object)
			spec, _ := podSpec(manifest{kind: kind, value: object})
			switch variant {
			case "volume source":
				for _, raw := range sliceField(spec, "volumes") {
					volume := raw.(map[string]any)
					if volume[keyName] == peersVolume {
						volume[keyConfigMap] = map[string]any{keyName: "attacker"}
					}
				}
			case "mount path", "GATEWAY_PEERS_FILE":
				for _, raw := range sliceField(spec, keyContainers) {
					container := raw.(map[string]any)
					for _, raw := range sliceField(container, "volumeMounts") {
						mount := raw.(map[string]any)
						if variant == "mount path" && mount[keyName] == peersVolume {
							mount["mountPath"] = "/attacker"
						}
					}
					for _, raw := range sliceField(container, "env") {
						variable := raw.(map[string]any)
						name, _ := variable[keyName].(string)
						if variant == "GATEWAY_PEERS_FILE" && strings.HasSuffix(name, "GATEWAY_PEERS_FILE") {
							variable["value"] = "/attacker"
						}
					}
				}
			}
			overlay := filepath.Join(root, "services", "sibling", "overlays", options.Environment)
			require.NoError(t, writeOverlayResource(overlay, "copy.yaml", object))
			assertIdentityTreeRefused(t, root, options, kind+" "+options.Namespace+"/", "services/api/", "services/sibling/")
		})
	}
}

func TestPromotedTreeRetainsCompleteOwnedBytes(t *testing.T) {
	for _, kind := range []string{kindDeployment, "ServiceAccount", "Role", "RoleBinding", kindConfigMap} {
		t.Run(kind, func(t *testing.T) {
			root, options := identityTreeFixture(t)
			var name string
			for _, doc := range options.workloadIdentityChecks[0].objects {
				if doc.kind == kind {
					name = metadataString(doc.value, keyName)
					break
				}
			}
			require.NotEmpty(t, name)
			appendIdentityPatch(t, filepath.Join(root, "services", "api"), options.Environment, kind, name, "- op: add\n  path: /metadata/annotations\n  value: {changed: later}\n")
			assertIdentityTreeRefused(t, root, options, "differs from its owning service render", "services/api/")
		})
	}
}

func TestPromotedTreeIdentitiesAfterKustomize(t *testing.T) {
	for _, transform := range []string{"namePrefix: pre-", "nameSuffix: -post", "namespace: target"} {
		t.Run(transform, func(t *testing.T) {
			root := t.TempDir()
			targetName := "object"
			if strings.HasPrefix(transform, "namePrefix") {
				targetName = "pre-object"
			}
			if strings.HasPrefix(transform, "nameSuffix") {
				targetName = "object-post"
			}
			for _, unit := range []string{"first", "second"} {
				overlay := filepath.Join(root, "services", unit, "overlays", "prod")
				require.NoError(t, os.MkdirAll(overlay, 0o755))
				name := "object"
				extra := transform + "\n"
				namespace := ""
				if unit == "second" {
					name = targetName
					extra = ""
					namespace = "  namespace: target\n"
				}
				require.NoError(t, os.WriteFile(filepath.Join(overlay, "object.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: "+name+"\n"+namespace), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(overlay, kustomizationFile), []byte("resources: [object.yaml]\n"+extra), 0o600))
			}
			_, err := validateTree(root, &RenderOptions{Promotable: true, Namespace: "target"})
			require.ErrorContains(t, err, "must occur exactly once")
			require.ErrorContains(t, err, "services/first/")
			require.ErrorContains(t, err, "services/second/")
		})
	}
}

func TestPromotedIdentityTreeOrdinaryPasses(t *testing.T) {
	root, options := identityTreeFixture(t)
	options.Destination = filepath.Join(t.TempDir(), "promoted")
	_, err := RenderOwnedTree(t.Context(), options, func(_ context.Context, stage string) error { return copyTree(root, stage) })
	require.NoError(t, err)
}

func TestAppliedOverlayRemainsVisibleWhenAnotherOverlayReferencesIt(t *testing.T) {
	root := t.TempDir()
	for _, service := range []string{"first", "second"} {
		overlay := filepath.Join(root, "services", service, "overlays", "prod")
		require.NoError(t, os.MkdirAll(overlay, 0o755))
		resource := "../../../first/overlays/prod"
		if service == "first" {
			resource = "object.yaml"
			require.NoError(t, os.WriteFile(filepath.Join(overlay, resource), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n"), 0o600))
		}
		require.NoError(t, os.WriteFile(filepath.Join(overlay, kustomizationFile), []byte("resources: ["+resource+"]\n"), 0o600))
	}
	_, err := validateTree(root, &RenderOptions{Environment: "prod", Namespace: "target", Promotable: true})
	require.ErrorContains(t, err, "must occur exactly once")
	require.ErrorContains(t, err, "services/first/")
	require.ErrorContains(t, err, "services/second/")
}

func TestObjectIdentityComparesAPIGroupInsteadOfVersion(t *testing.T) {
	for _, sameGroup := range []bool{false, true} {
		t.Run(fmt.Sprint(sameGroup), func(t *testing.T) {
			root := t.TempDir()
			secondGroup := "other.example"
			if sameGroup {
				secondGroup = "objects.example"
			}
			for i, api := range []string{"objects.example/v1", secondGroup + "/v2"} {
				require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("object-%d.yaml", i)), []byte("apiVersion: "+api+"\nkind: Widget\nmetadata:\n  name: same\n  namespace: target\n"), 0o600))
			}
			_, err := validateTree(root, &RenderOptions{Namespace: "target", Promotable: true})
			if sameGroup {
				require.ErrorContains(t, err, "must occur exactly once")
				require.ErrorContains(t, err, "object-0.yaml")
				require.ErrorContains(t, err, "object-1.yaml")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
