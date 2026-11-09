package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const digestImage = "registry.example/api@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// digestUnit writes a service unit the way an agent and the configuration
// projections render it: a Deployment and a StatefulSet that read the
// unit's ConfigMap and its ExternalSecret-materialized Secret, a Job that
// reads both too, and a Deployment that reads nothing, with the ConfigMap and
// the ExternalSecret in the environment overlay.
func digestUnit(t *testing.T, root, configValue, remoteKey string) {
	t.Helper()
	base := filepath.Join(root, "services", "api", "base")
	overlay := filepath.Join(root, "services", "api", "overlays", "prod")
	require.NoError(t, os.MkdirAll(base, 0o755))
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	podSpec := `
      containers:
        - name: api
          image: ` + digestImage + `
          envFrom:
            - configMapRef:
                name: cm-api
          env:
            - name: DATABASE_CONNECTION
              valueFrom:
                secretKeyRef:
                  name: secret-api
                  key: DATABASE_CONNECTION
`
	workloads := `apiVersion: apps/v1
kind: Deployment
metadata:
  namespace: shop
  name: api
spec:
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
    spec:` + podSpec + `---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  namespace: shop
  name: store
spec:
  serviceName: store
  selector:
    matchLabels:
      app: store
  template:
    metadata:
      labels:
        app: store
    spec:` + podSpec + `---
apiVersion: batch/v1
kind: Job
metadata:
  namespace: shop
  name: migrate
spec:
  template:
    metadata:
      labels:
        app: migrate
    spec:
      restartPolicy: Never` + podSpec + `---
apiVersion: apps/v1
kind: Deployment
metadata:
  namespace: shop
  name: static
spec:
  selector:
    matchLabels:
      app: static
  template:
    metadata:
      labels:
        app: static
    spec:
      containers:
        - name: static
          image: ` + digestImage + `
`
	require.NoError(t, os.WriteFile(filepath.Join(base, "workloads.yaml"), []byte(workloads), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - workloads.yaml\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "configmap.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  namespace: shop\n  name: cm-api\ndata:\n  FEATURE: \""+configValue+"\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "external-secret.yaml"), []byte(`apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  namespace: shop
  name: secret-api
spec:
  secretStoreRef:
    name: cell-secrets
    kind: ClusterSecretStore
  target:
    name: secret-api
  data:
    - secretKey: DATABASE_CONNECTION
      remoteRef:
        key: `+remoteKey+`
        property: DATABASE_CONNECTION
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: shop\nresources:\n  - ../../base\n  - configmap.yaml\n  - external-secret.yaml\n"), 0o644))
}

func digestRender() *RenderOptions {
	return &RenderOptions{
		Module: "shop", Environment: "prod",
		Units: []InventoryUnit{{Kind: UnitKindService, Module: "shop", Name: "api", Path: "services/api"}},
	}
}

// stampedTemplates renders the unit's configuration digest and returns each
// workload's pod-template annotations, by kind and name.
func stampedTemplates(t *testing.T, root string) map[string]map[string]any {
	t.Helper()
	require.NoError(t, projectConfigurationDigest(testRenderRoot(t, root), root, digestRender()))
	manifests, err := overlayManifests(filepath.Join(root, "services", "api", "overlays", "prod"))
	require.NoError(t, err)
	templates := map[string]map[string]any{}
	for _, item := range manifests {
		if template := podTemplate(item); template != nil {
			templates[item.kind+"/"+metadataString(item.value, keyName)] = mapField(mapField(template, keyMetadata), "annotations")
		}
	}
	return templates
}

func renderedOverlay(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "services", "api", "overlays", "prod", "kustomization.yaml"))
	require.NoError(t, err)
	return string(data)
}

func TestTheSameConfigurationRendersTheSameDigest(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	digestUnit(t, first, "on", "shop-api")
	digestUnit(t, second, "on", "shop-api")

	firstTemplates, secondTemplates := stampedTemplates(t, first), stampedTemplates(t, second)
	digest, _ := firstTemplates["Deployment/api"][ConfigurationDigestAnnotation].(string)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)
	require.Equal(t, firstTemplates, secondTemplates)
	require.Equal(t, renderedOverlay(t, first), renderedOverlay(t, second), "identical inputs render byte-identical overlays")
}

func TestAChangedConfigMapValueChangesThePodTemplate(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	digestUnit(t, before, "on", "shop-api")
	digestUnit(t, after, "off", "shop-api")

	beforeTemplates, afterTemplates := stampedTemplates(t, before), stampedTemplates(t, after)
	for _, workload := range []string{"Deployment/api", "StatefulSet/store"} {
		require.Contains(t, beforeTemplates[workload], ConfigurationDigestAnnotation, workload)
		require.NotEqual(t, beforeTemplates[workload][ConfigurationDigestAnnotation], afterTemplates[workload][ConfigurationDigestAnnotation],
			"%s: a configuration-only change must change the pod template, or the pods keep the old configuration", workload)
	}
}

func TestAChangedSecretReferenceChangesThePodTemplate(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	digestUnit(t, before, "on", "shop-api")
	digestUnit(t, after, "on", "shop-api-rotated")

	require.NotEqual(t,
		stampedTemplates(t, before)["Deployment/api"][ConfigurationDigestAnnotation],
		stampedTemplates(t, after)["Deployment/api"][ConfigurationDigestAnnotation])
}

func TestASecretValueNeverEntersTheDigest(t *testing.T) {
	render := func(value string) (string, string) {
		root := t.TempDir()
		digestUnit(t, root, "on", "shop-api")
		overlay := filepath.Join(root, "services", "api", "overlays", "prod")
		// A Secret the overlay renders itself, standing in for the
		// ExternalSecret: its value must move nothing the render writes.
		require.NoError(t, os.WriteFile(filepath.Join(overlay, "external-secret.yaml"),
			[]byte("apiVersion: v1\nkind: Secret\nmetadata:\n  namespace: shop\n  name: secret-api\nstringData:\n  DATABASE_CONNECTION: "+value+"\n"), 0o644))
		digest, _ := stampedTemplates(t, root)["Deployment/api"][ConfigurationDigestAnnotation].(string)
		return digest, renderedOverlay(t, root)
	}
	first, firstOverlay := render("postgres://u:first-password@db/x")
	second, secondOverlay := render("postgres://u:second-password@db/x")
	require.NotEmpty(t, first)
	require.Equal(t, first, second, "a Secret's value is never read into the digest")
	for _, overlay := range []string{firstOverlay, secondOverlay} {
		require.False(t, strings.Contains(overlay, "password"), "no secret value reaches the overlay")
	}
}

func TestOnlyAServingWorkloadThatReadsConfigurationIsStamped(t *testing.T) {
	root := t.TempDir()
	digestUnit(t, root, "on", "shop-api")
	templates := stampedTemplates(t, root)

	require.NotContains(t, templates["Job/migrate"], ConfigurationDigestAnnotation, "a Job's pod template is immutable; it reads the configuration current when it starts")
	require.NotContains(t, templates["Deployment/static"], ConfigurationDigestAnnotation, "a workload that reads no configuration has nothing to digest")
	require.Equal(t, templates["Deployment/api"][ConfigurationDigestAnnotation], templates["StatefulSet/store"][ConfigurationDigestAnnotation],
		"two workloads reading the same configuration carry the same digest")
}

func TestTheConfigurationDigestAnnotationIsTheRenders(t *testing.T) {
	root := t.TempDir()
	digestUnit(t, root, "on", "shop-api")
	path := filepath.Join(root, "services", "api", "base", "workloads.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	authored := strings.Replace(string(data), "      labels:\n        app: api\n", "      labels:\n        app: api\n      annotations:\n        "+ConfigurationDigestAnnotation+": sha256:authored\n", 1)
	require.NoError(t, os.WriteFile(path, []byte(authored), 0o644))

	err = projectConfigurationDigest(testRenderRoot(t, root), root, digestRender())
	require.ErrorContains(t, err, "which the render owns")
}

func TestThePodReadsEveryConfigurationSource(t *testing.T) {
	configMaps, secrets := podConfigurationReferences(map[string]any{
		"initContainers": []any{map[string]any{"envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "init-secret"}}}}},
		"containers": []any{map[string]any{
			"envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": "cm-a"}}},
			"env":     []any{map[string]any{"valueFrom": map[string]any{"configMapKeyRef": map[string]any{"name": "cm-b"}}}},
		}},
		"volumes": []any{
			map[string]any{"configMap": map[string]any{"name": "cm-c"}},
			map[string]any{"secret": map[string]any{"secretName": "volume-secret"}},
			map[string]any{"projected": map[string]any{"sources": []any{
				map[string]any{"configMap": map[string]any{"name": "cm-a"}},
				map[string]any{"secret": map[string]any{"name": "projected-secret"}},
			}}},
		},
	})
	require.Equal(t, []string{"cm-a", "cm-b", "cm-c"}, configMaps)
	require.Equal(t, []string{"init-secret", "projected-secret", "volume-secret"}, secrets)
}

func TestARenderStampsTheConfigurationDigest(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "tree")
	opts := digestRender()
	opts.Destination = destination
	_, err := RenderOwnedTree(t.Context(), opts, func(_ context.Context, stage string) error {
		digestUnit(t, stage, "on", "shop-api")
		return nil
	})
	require.NoError(t, err)
	require.Contains(t, renderedOverlay(t, destination), ConfigurationDigestAnnotation+": sha256:", "the render itself, not only the projection, stamps the pod template")
}
