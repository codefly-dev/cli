package gitops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

const waypointLabel, waypointNamespaceLabel = "istio.io/use-waypoint", "istio.io/use-waypoint-namespace"

// serviceUnit writes a service unit as an agent renders it: a Service in the
// base and an environment overlay that is a kustomization over it.
func serviceUnit(t *testing.T, root, service string) {
	t.Helper()
	base := filepath.Join(root, "services", service, "base")
	overlay := filepath.Join(root, "services", service, "overlays", "prod")
	require.NoError(t, os.MkdirAll(base, 0o755))
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "service.yaml"), []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: "+service+"\n  labels:\n    app: "+service+"\nspec:\n  selector:\n    app: "+service+"\n  ports:\n    - name: http\n      port: 8080\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - service.yaml\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"), 0o644))
}

func meshRender(labels map[string]string) *RenderOptions {
	return &RenderOptions{
		Module: "shop", Environment: "prod",
		Units: []InventoryUnit{
			{Kind: UnitKindService, Module: "shop", Name: "gateway", Path: "services/gateway"},
			{Kind: UnitKindService, Module: "shop", Name: "api", Path: "services/api"},
		},
		Target: &environments.Environment{
			Name: "prod",
			Cell: map[string]environments.EnvironmentCellWorkload{"shop/gateway": {Presenter: []string{"workload"}}},
			ServiceMesh: &environments.EnvironmentServiceMesh{Services: map[string]environments.EnvironmentMeshService{
				"shop/gateway": {Labels: labels},
			}},
		},
	}
}

func renderedServiceLabels(t *testing.T, root, service string) map[string]any {
	t.Helper()
	manifests, err := overlayManifests(filepath.Join(root, "services", service, "overlays", "prod"))
	require.NoError(t, err)
	for _, item := range manifests {
		if item.kind == kindService {
			return mapField(mapField(item.value, keyMetadata), "labels")
		}
	}
	t.Fatalf("%s renders no Service", service)
	return nil
}

func TestServiceMeshLabelsThePresenterServiceAlone(t *testing.T) {
	root := t.TempDir()
	serviceUnit(t, root, "gateway")
	serviceUnit(t, root, "api")
	base, err := os.ReadFile(filepath.Join(root, "services", "gateway", "base", "service.yaml"))
	require.NoError(t, err)

	opts := meshRender(map[string]string{waypointLabel: "shop-prod-waypoint", waypointNamespaceLabel: "istio-waypoint"})
	require.NoError(t, opts.Target.Validate())
	require.NoError(t, projectServiceMesh(testRenderRoot(t, root), root, opts))

	require.Equal(t, map[string]any{"app": "gateway", waypointLabel: "shop-prod-waypoint", waypointNamespaceLabel: "istio-waypoint"},
		renderedServiceLabels(t, root, "gateway"))
	require.Equal(t, map[string]any{"app": "api"}, renderedServiceLabels(t, root, "api"), "no other Service carries either label")
	after, err := os.ReadFile(filepath.Join(root, "services", "gateway", "base", "service.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(base), string(after), "the base the agent rendered is untouched")
}

func TestServiceMeshRefusesALabelTheRenderedServiceCarries(t *testing.T) {
	root := t.TempDir()
	serviceUnit(t, root, "gateway")
	serviceUnit(t, root, "api")
	err := projectServiceMesh(testRenderRoot(t, root), root, meshRender(map[string]string{"app": "other"}))
	require.ErrorContains(t, err, `service-mesh shop/gateway: Service gateway already carries label "app"`)
}

func TestServiceMeshRefusesAServiceTheModuleDoesNotRender(t *testing.T) {
	root := t.TempDir()
	serviceUnit(t, root, "api")
	opts := meshRender(map[string]string{waypointLabel: "w"})
	opts.Units = opts.Units[1:]
	require.ErrorContains(t, projectServiceMesh(testRenderRoot(t, root), root, opts), "service-mesh shop/gateway: module shop renders no service gateway")
}
