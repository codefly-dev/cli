package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

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
			_, err := validateTree(root, &RenderOptions{Promotable: true, Namespace: "target", Environment: "prod", UnitNames: []string{"first", "second"}})
			require.ErrorContains(t, err, "must occur exactly once")
			require.ErrorContains(t, err, "services/first/")
			require.ErrorContains(t, err, "services/second/")
		})
	}
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
	_, err := validateTree(root, &RenderOptions{Environment: "prod", Namespace: "target", Promotable: true, UnitNames: []string{"first", "second"}})
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

// twoUnitTree stages two service units, api and sibling, each rendering its
// own workload in the namespace the options name, and checks the tree is
// promotable as it stands.
func twoUnitTree(t *testing.T) (string, *RenderOptions) {
	t.Helper()
	root := t.TempDir()
	options := &RenderOptions{Module: "app", Environment: "prod", Namespace: "example-app", Promotable: true, CheckUnitDirectories: true}
	for _, name := range []string{"api", "sibling"} {
		stage := t.TempDir()
		body := strings.ReplaceAll(sidecarDeployment, "name: api", "name: "+name)
		body = strings.ReplaceAll(body, "serviceAccountName: api", "serviceAccountName: "+name)
		body = strings.Replace(body, "metadata:\n", "metadata:\n  namespace: "+options.Namespace+"\n", 1)
		require.NoError(t, kustomizedWorkload(body)(t.Context(), stage))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "services"), 0o755))
		require.NoError(t, os.Rename(filepath.Join(stage, "services", "api"), filepath.Join(root, "services", name)))
		options.Units = append(options.Units, InventoryUnit{Module: "app", Name: name, Kind: UnitKindService, Path: "services/" + name, Output: restrictedRenderEvidence()})
	}
	_, err := validateTree(root, options)
	require.NoError(t, err)
	return root, options
}

// A Kustomize transform that strips an object's namespace defaults it back to
// the overlay's, so a sibling unit's copy of another unit's object collides
// with it after the build, whatever the source file said.
func TestPromotedTreeRefusesNamespaceRemovalAfterTransform(t *testing.T) {
	for _, kind := range []string{kindConfigMap, kindDeployment} {
		t.Run(kind, func(t *testing.T) {
			root, options := twoUnitTree(t)
			built, err := overlayManifests(filepath.Join(root, "services", "api", "overlays", options.Environment))
			require.NoError(t, err)
			var object map[string]any
			for _, doc := range built {
				if doc.kind == kind {
					data, err := yaml.Marshal(doc.value)
					require.NoError(t, err)
					require.NoError(t, yaml.Unmarshal(data, &object))
					break
				}
			}
			if object == nil {
				require.Equal(t, kindConfigMap, kind, "the fixture renders a Deployment")
				object = map[string]any{keyAPIVersion: "v1", keyKind: kindConfigMap, keyMetadata: map[string]any{keyName: "api-settings", keyNamespace: options.Namespace}, dataKey: map[string]any{"mode": "prod"}}
				apiOverlay := filepath.Join(root, "services", "api", "overlays", options.Environment)
				require.NoError(t, writeOverlayResource(apiOverlay, "settings.yaml", object))
				_, err = validateTree(root, options)
				require.NoError(t, err)
			}
			overlay := filepath.Join(root, "services", "sibling", "overlays", options.Environment)
			require.NoError(t, writeOverlayResource(overlay, "collision.yaml", object))
			name := metadataString(object, keyName)
			data, err := os.ReadFile(filepath.Join(overlay, kustomizationFile))
			require.NoError(t, err)
			data = append(data, []byte("namespace: other\npatchesJson6902:\n- target:\n    version: v1\n    kind: "+kind+"\n    name: "+name+"\n  patch: |-\n    - op: remove\n      path: /metadata/namespace\n")...)
			if kind == kindDeployment {
				data = []byte(strings.Replace(string(data), "    version: v1", "    group: apps\n    version: v1", 1))
			}
			require.NoError(t, os.WriteFile(filepath.Join(overlay, kustomizationFile), data, 0o600))
			built, err = overlayManifests(overlay)
			require.NoError(t, err)
			for _, doc := range built {
				if doc.kind == kind && metadataString(doc.value, keyName) == name {
					require.Empty(t, metadataString(doc.value, keyNamespace))
				}
			}
			_, err = validateTree(root, options)
			require.ErrorContains(t, err, "must occur exactly once")
			require.ErrorContains(t, err, kind+" "+options.Namespace+"/"+name)
		})
	}
}

// assertTreeRefused checks a tree is refused both by validation and by a
// full owned-tree render, naming every path given, and that the refused
// render left no destination behind.
func assertTreeRefused(t *testing.T, root string, options *RenderOptions, paths ...string) {
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

func TestPromotedTreeRefusesNamespaceFreeObjectsAcrossDestinations(t *testing.T) {
	root := t.TempDir()
	options := &RenderOptions{Environment: "prod", Namespace: "target", AppProject: "project", Promotable: true, CheckUnitDirectories: true}
	for _, namespace := range []string{"alpha", "beta"} {
		overlay := filepath.Join(root, "services", namespace, "overlays", "prod")
		require.NoError(t, os.MkdirAll(overlay, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(overlay, "policy.yaml"), []byte("apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingAdmissionPolicy\nmetadata:\n  name: shared\nspec:\n  validations:\n  - expression: \"object.metadata.name != '"+namespace+"'\"\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(overlay, kustomizationFile), []byte("resources: [policy.yaml]\nnamespace: "+namespace+"\n"), 0o600))
		options.Units = append(options.Units, InventoryUnit{Kind: UnitKindService, Name: namespace, Path: "services/" + namespace, Output: restrictedRenderEvidence()})
		writeTestApplication(t, root, namespace, "services/"+namespace+"/overlays/prod", namespace)
	}
	assertTreeRefused(t, root, options, "ValidatingAdmissionPolicy /shared", "must occur exactly once")
}

func writeTestApplication(t *testing.T, root, name, source, namespace string) {
	t.Helper()
	data, err := yaml.Marshal(map[string]any{
		keyAPIVersion: argoAPIVersion, keyKind: kindApplication,
		keyMetadata: map[string]any{keyName: name, keyNamespace: argoNamespace},
		keySpec:     map[string]any{"project": "project", "source": map[string]any{"path": source}, "destination": map[string]any{keyNamespace: namespace}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, name+"-application.yaml"), data, 0o600))
}

func TestPromotedSingleUnitWithSharedOverlay(t *testing.T) {
	for _, component := range []bool{false, true} {
		t.Run(fmt.Sprint(component), func(t *testing.T) {
			root := t.TempDir()
			shared := filepath.Join(root, "shared", "overlays", "prod")
			unit := filepath.Join(root, "services", "api", "overlays", "prod")
			require.NoError(t, os.MkdirAll(shared, 0o755))
			require.NoError(t, os.MkdirAll(unit, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(shared, "config.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n"), 0o600))
			kind, version, reference := kindKustomization, "v1beta1", resourcesKey
			if component {
				kind, version, reference = "Component", "v1alpha1", "components"
			}
			require.NoError(t, os.WriteFile(filepath.Join(shared, kustomizationFile), []byte("apiVersion: kustomize.config.k8s.io/"+version+"\nkind: "+kind+"\nresources: [config.yaml]\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(unit, kustomizationFile), []byte(reference+": [../../../../shared/overlays/prod]\n"), 0o600))
			options := &RenderOptions{Environment: "prod", Namespace: "target", Promotable: true, CheckUnitDirectories: true, Units: []InventoryUnit{{Kind: UnitKindService, Name: "api", Path: "services/api", Output: restrictedRenderEvidence()}}}
			built, err := validateTree(root, options)
			require.NoError(t, err)
			require.Len(t, built, 1)
			options.Destination = filepath.Join(t.TempDir(), "promoted")
			_, err = RenderOwnedTree(t.Context(), options, func(_ context.Context, stage string) error { return copyTree(root, stage) })
			require.NoError(t, err)
		})
	}
}

func TestApplicationAppliesReferencedDirectory(t *testing.T) {
	for _, appSet := range []bool{false, true} {
		t.Run(fmt.Sprint(appSet), func(t *testing.T) {
			root := t.TempDir()
			unit := filepath.Join(root, "services", "api", "overlays", "prod")
			child := filepath.Join(unit, "child")
			require.NoError(t, os.MkdirAll(child, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(child, "config.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n  namespace: destination\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(child, kustomizationFile), []byte("resources: [config.yaml]\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(unit, kustomizationFile), []byte("resources: [child]\n"), 0o600))
			source := "services/api/overlays/prod/child"
			if appSet {
				data := "apiVersion: argoproj.io/v1alpha1\nkind: ApplicationSet\nmetadata:\n  name: child\n  namespace: argocd\nspec:\n  generators:\n  - list:\n      elements: [{name: child}]\n  template:\n    spec:\n      project: project\n      source:\n        path: " + source + "\n      destination:\n        namespace: destination\n"
				require.NoError(t, os.WriteFile(filepath.Join(root, "appset.yaml"), []byte(data), 0o600))
			} else {
				writeTestApplication(t, root, "child", source, "destination")
			}
			options := &RenderOptions{Environment: "prod", Namespace: "destination", Promotable: true, CheckUnitDirectories: true, Units: []InventoryUnit{{Kind: UnitKindService, Name: "api", Path: "services/api", Output: restrictedRenderEvidence()}}}
			assertTreeRefused(t, root, options, "must occur exactly once", "child")
		})
	}
}

func TestObjectIdentityUsesApplicationDestinationAndOutput(t *testing.T) {
	for _, appSet := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("appset=%t/explicit=%t", appSet, explicit), func(t *testing.T) {
				root := t.TempDir()
				overlay := filepath.Join(root, "services", "api", "overlays", "prod")
				require.NoError(t, os.MkdirAll(overlay, 0o755))
				namespace := ""
				if explicit {
					namespace = "  namespace: from-output\n"
				}
				require.NoError(t, os.WriteFile(filepath.Join(overlay, "object.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: object\n"+namespace), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(overlay, kustomizationFile), []byte("resources: [object.yaml]\n"), 0o600))
				if appSet {
					require.NoError(t, os.Mkdir(filepath.Join(root, "bootstrap"), 0o755))
					body := "apiVersion: argoproj.io/v1alpha1\nkind: ApplicationSet\nmetadata:\n  name: root\n  namespace: argocd\nspec:\n  goTemplate: true\n  generators:\n  - matrix:\n      generators:\n      - list:\n          elements: [{tenant: test}]\n      - list:\n          elements: [{overlay: services/api/overlays/prod, namespace: from-application}]\n  template:\n    spec:\n      source:\n        path: '{{ .overlay }}'\n      destination:\n        namespace: '{{ .namespace }}'\n"
					require.NoError(t, os.WriteFile(filepath.Join(root, bootstrapApplicationSetPath), []byte(body), 0o600))
				} else {
					writeTestApplication(t, root, "root", "services/api/overlays/prod", "from-application")
				}
				documents, err := validateTree(root, &RenderOptions{Environment: "prod", Namespace: "inventory-default", Promotable: true, UnitNames: []string{"api"}})
				require.NoError(t, err)
				matched := 0
				for _, doc := range documents {
					if doc.kind != kindConfigMap {
						continue
					}
					matched++
					expected := "from-application"
					if explicit {
						expected = "from-output"
					}
					require.Equal(t, expected, doc.identity("").namespace)
				}
				require.Equal(t, 1, matched)
			})
		}
	}
}
