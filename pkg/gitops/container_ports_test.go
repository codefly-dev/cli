package gitops

import (
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const overlayKustomization = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"

func writeUnit(t *testing.T, manifests string, overlay map[string]string) string {
	t.Helper()
	unit := t.TempDir()
	files := map[string]string{
		filepath.Join("base", "kustomization.yaml"):                "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - manifests.yaml\n",
		filepath.Join("base", "manifests.yaml"):                    manifests,
		filepath.Join("overlays", "staging", "kustomization.yaml"): overlayKustomization,
	}
	for path, content := range overlay {
		files[filepath.Join("overlays", "staging", path)] = content
	}
	writeFiles(t, unit, files)
	return unit
}

// A redis-shaped unit: one Service selecting primary and replica pods by a set
// label, each port targeting a container port by name.
const namedTargetUnit = `apiVersion: v1
kind: Service
metadata:
  name: cache
spec:
  selector:
    set: cache
  ports:
    - name: write
      port: 41000
      targetPort: primary
    - name: read
      port: 41001
      targetPort: replica
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: cache
spec:
  template:
    metadata:
      labels:
        set: cache
        role: primary
    spec:
      containers:
        - name: redis
          ports:
            - name: primary
              containerPort: 6379
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: cache-replica
spec:
  template:
    metadata:
      labels:
        set: cache
        role: replica
    spec:
      containers:
        - name: redis
          ports:
            - name: replica
              containerPort: 6380
---
apiVersion: v1
kind: Service
metadata:
  name: unrelated
spec:
  selector:
    app: other
  ports:
    - port: 9999
`

func TestRenderedContainerPortsResolvesNamedTargetsThroughSelectedPods(t *testing.T) {
	unit := writeUnit(t, namedTargetUnit, nil)
	ports, err := RenderedContainerPorts(unit, "staging", map[string]uint32{"write": 41000, "read": 41001, "absent": 41002, "other": 9999})
	require.NoError(t, err)
	// A Service port with no targetPort targets itself; an endpoint no Service
	// publishes is absent.
	require.Equal(t, map[string]uint32{"write": 6379, "read": 6380, "other": 9999}, ports)
}

func TestRenderedContainerPortsHonoursTheEnvironmentOverlay(t *testing.T) {
	unit := writeUnit(t, namedTargetUnit, map[string]string{
		"kustomization.yaml": overlayKustomization + "patches:\n  - path: port.yaml\n",
		"port.yaml":          "apiVersion: v1\nkind: Service\nmetadata:\n  name: cache\nspec:\n  ports:\n    - port: 41000\n      targetPort: 7000\n",
	})
	ports, err := RenderedContainerPorts(unit, "staging", map[string]uint32{"write": 41000})
	require.NoError(t, err)
	require.Equal(t, map[string]uint32{"write": 7000}, ports)
}

func TestRenderedContainerPortsRefusesAmbiguousOrUnresolvableTargets(t *testing.T) {
	disagreeing := namedTargetUnit + `---
apiVersion: v1
kind: Service
metadata:
  name: cache-headless
spec:
  selector:
    set: cache
  ports:
    - port: 41000
      targetPort: 6390
`
	_, err := RenderedContainerPorts(writeUnit(t, disagreeing, nil), "staging", map[string]uint32{"write": 41000})
	require.ErrorContains(t, err, "endpoint write: Services publishing port 41000 target different container ports: 6379 (Service cache); 6390 (Service cache-headless)")

	unresolved := `apiVersion: v1
kind: Service
metadata:
  name: api
spec:
  selector:
    app: api
  ports:
    - port: 8080
      targetPort: http
`
	_, err = RenderedContainerPorts(writeUnit(t, unresolved, nil), "staging", map[string]uint32{"rest": 8080})
	require.ErrorContains(t, err, `endpoint rest: rendered Service api port 8080 targets container port "http", which no pod it selects declares`)

	_, err = RenderedContainerPorts(t.TempDir(), "staging", map[string]uint32{"rest": 8080})
	require.ErrorContains(t, err, "build ")
}

func TestDeclaredEndpointPorts(t *testing.T) {
	declared, err := declaredEndpointPorts(&resources.Service{Name: "api"})
	require.NoError(t, err)
	require.Nil(t, declared)

	declared, err = declaredEndpointPorts(&resources.Service{Name: "api", Spec: map[string]any{
		"deployment": map[string]any{"endpoint-ports": map[string]any{"grpc": 9090, "rest": 8080}, "public-egress-ports": []any{443}},
	}})
	require.NoError(t, err)
	require.Equal(t, map[string]uint32{"grpc": 9090, "rest": 8080}, declared)

	for name, spec := range map[string]any{
		"not a number": map[string]any{"endpoint-ports": map[string]any{"grpc": "ninety"}},
		"not a map":    map[string]any{"endpoint-ports": []any{9090}},
		"out of range": map[string]any{"endpoint-ports": map[string]any{"grpc": 70000}},
		"zero":         map[string]any{"endpoint-ports": map[string]any{"grpc": 0}},
	} {
		_, err = declaredEndpointPorts(&resources.Service{Name: "api", Spec: map[string]any{"deployment": spec}})
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "service api: spec.deployment.endpoint-ports", name)
	}
}

// A declaration is compared only when present; with one, a Service that does
// not publish the endpoint's in-cluster port is refused rather than skipped.
func TestVerifyDeclaredEndpointPortsRefusesAnUnpublishedEndpoint(t *testing.T) {
	unit := writeUnit(t, namedTargetUnit, nil)
	service := &resources.Service{Name: "cache"}
	require.NoError(t, verifyDeclaredEndpointPorts(unit, "staging", "shop", service, map[string]uint32{"write": 1}))

	service.Spec = map[string]any{"deployment": map[string]any{"endpoint-ports": map[string]any{"write": 6379, "read": 6380}}}
	require.NoError(t, verifyDeclaredEndpointPorts(unit, "staging", "shop", service, map[string]uint32{"write": 41000, "read": 41001}))

	err := verifyDeclaredEndpointPorts(unit, "staging", "shop", service, map[string]uint32{"write": 41000, "read": 50000})
	require.EqualError(t, err, "service shop/cache: spec.deployment.endpoint-ports disagrees with the manifests its agent rendered for environment staging:\n"+
		"  read = 6380, but no rendered Service publishes its in-cluster port 50000")
}
