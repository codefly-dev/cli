package gitops

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const gatewayServiceUnit = `apiVersion: v1
kind: Service
metadata:
  name: selected-gateway
spec:
  selector:
    app: selected-gateway
  ports:
    - name: rest
      port: 80
      targetPort: 8080
      appProtocol: http
    - name: workload
      port: 81
      targetPort: 8081
      appProtocol: http
`

func gatewayMeshRecord() *MeshEdges {
	return &MeshEdges{Gateway: "host/gateway", Edges: []MeshEdge{
		{Module: "host", Service: "gateway", Namespace: "cell-host", Endpoint: "rest", Port: 80, TargetPort: 8080, Ingress: true},
		{Module: "host", Service: "gateway", Namespace: "cell-host", Endpoint: "workload", Port: 81, TargetPort: 8081},
	}}
}

func bindGatewayTestUnit(t *testing.T, record *MeshEdges, manifests string, overlay map[string]string) error {
	t.Helper()
	unit := writeUnit(t, manifests, overlay)
	tree, err := newRenderRoot(unit)
	require.NoError(t, err)
	defer tree.Close()
	return bindGatewayServiceEvidence(record, tree, unit, "staging", []InventoryUnit{{Kind: UnitKindService, Name: "gateway", Path: "."}}, "")
}

func bindGatewayTestUnitCallingThrough(t *testing.T, record *MeshEdges, manifests, callEndpoint string) error {
	t.Helper()
	unit := writeUnit(t, manifests, nil)
	tree, err := newRenderRoot(unit)
	require.NoError(t, err)
	defer tree.Close()
	return bindGatewayServiceEvidence(record, tree, unit, "staging", []InventoryUnit{{Kind: UnitKindService, Name: "gateway", Path: "."}}, callEndpoint)
}

const gatewayHopPort = `    - name: http-hop
      port: 82
      targetPort: 8082
`

func gatewayHopEdge() MeshEdge {
	return MeshEdge{Module: "host", Service: "gateway", Namespace: "cell-host", Endpoint: "hop", Port: 82, TargetPort: 8082,
		Principals: []string{"spiffe://cluster.local/ns/cell-runtime/sa/worker"}}
}

// An environment that sends every prepared runnable call to one of the
// gateway's own endpoints (runnable-call-endpoint: host/gateway/hop) makes
// that endpoint a third mesh endpoint of the gateway: the executors reach it,
// on a Service port of its own. Without the declaration the same edge is a
// listener around the presenter path, and is refused as before.
func TestGatewayMeshEvidenceAdmitsTheDeclaredRunnableCallEndpoint(t *testing.T) {
	manifests := gatewayServiceUnit + gatewayHopPort
	t.Run("declared on the gateway", func(t *testing.T) {
		record := gatewayMeshRecord()
		record.Edges = append(record.Edges, gatewayHopEdge())
		require.NoError(t, bindGatewayTestUnitCallingThrough(t, record, manifests, "host/gateway/hop"))
		require.Equal(t, "http-hop", record.Edges[2].RenderedService.PortName)
	})
	t.Run("not declared", func(t *testing.T) {
		record := gatewayMeshRecord()
		record.Edges = append(record.Edges, gatewayHopEdge())
		err := bindGatewayTestUnitCallingThrough(t, record, manifests, "")
		require.ErrorContains(t, err, "gateway mesh endpoint hop cannot bypass its HTTP presenter path")
		require.ErrorContains(t, err, "rest, workload")
	})
	t.Run("declared on another service", func(t *testing.T) {
		record := gatewayMeshRecord()
		record.Edges = append(record.Edges, gatewayHopEdge())
		require.ErrorContains(t, bindGatewayTestUnitCallingThrough(t, record, manifests, "runtime/worker/hop"), "cannot bypass")
	})
	t.Run("declared but no executor reaches it", func(t *testing.T) {
		require.NoError(t, bindGatewayTestUnitCallingThrough(t, gatewayMeshRecord(), manifests, "host/gateway/hop"))
	})
	t.Run("shares the presenter's listener", func(t *testing.T) {
		record := gatewayMeshRecord()
		edge := gatewayHopEdge()
		edge.TargetPort = record.Edges[0].TargetPort
		record.Edges = append(record.Edges, edge)
		require.ErrorContains(t, bindGatewayTestUnitCallingThrough(t, record, manifests, "host/gateway/hop"), "distinct resolved container ports")
	})
}

func TestGatewayMeshEvidenceUsesTheEffectiveService(t *testing.T) {
	record := gatewayMeshRecord()
	err := bindGatewayTestUnit(t, record, gatewayServiceUnit, map[string]string{
		"kustomization.yaml": overlayKustomization + "namePrefix: deployment-\ncommonLabels:\n  cell: selected\n",
	})
	require.NoError(t, err)
	for _, edge := range record.Edges {
		require.Equal(t, &MeshRenderedService{
			Name: "deployment-selected-gateway", Selector: map[string]string{"app": "selected-gateway", "cell": "selected"},
			PortName: edge.Endpoint, Protocol: "TCP", AppProtocol: "http",
		}, edge.RenderedService)
	}
}

func TestGatewayMeshEvidenceRefusesBypassAndAmbiguity(t *testing.T) {
	for name, manifests := range map[string]string{
		"missing HTTP protocol": strings.ReplaceAll(gatewayServiceUnit, "      appProtocol: http\n", ""),
		"TCP passthrough":       strings.ReplaceAll(gatewayServiceUnit, "appProtocol: http", "appProtocol: tcp"),
		"UDP":                   strings.ReplaceAll(gatewayServiceUnit, "appProtocol: http", "appProtocol: http\n      protocol: UDP"),
		"empty selector":        strings.Replace(gatewayServiceUnit, "    app: selected-gateway\n", "", 1),
		"wrong namespace":       strings.Replace(gatewayServiceUnit, "  name: selected-gateway", "  name: selected-gateway\n  namespace: other", 1),
		"unnamed port":          strings.Replace(gatewayServiceUnit, "name: workload", "description: workload", 1),
		"alternate service":     gatewayServiceUnit + "---\n" + strings.Replace(gatewayServiceUnit, "name: selected-gateway", "name: alternate", 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, bindGatewayTestUnit(t, gatewayMeshRecord(), manifests, nil))
		})
	}
	t.Run("one listener cannot carry public and workload traffic", func(t *testing.T) {
		record := gatewayMeshRecord()
		record.Edges[1].TargetPort = record.Edges[0].TargetPort
		require.ErrorContains(t, bindGatewayTestUnit(t, record, gatewayServiceUnit, nil), "distinct resolved container ports")
	})
	t.Run("missing workload endpoint", func(t *testing.T) {
		record := gatewayMeshRecord()
		record.Edges = record.Edges[:1]
		require.ErrorContains(t, bindGatewayTestUnit(t, record, gatewayServiceUnit, nil), "distinct resolved container ports")
	})
}

// A gateway Service port that carries HTTP by the mesh's port-name convention
// (`http-<suffix>`, what the service agents render) is evidence; a port that
// says neither appProtocol nor an http name is not, and one that says another
// appProtocol is refused even under an http name.
func TestGatewayServicePortSaysHTTPByNameOrAppProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, portName, appProtocol string
		ok                          bool
	}{
		{"explicit appProtocol", "rest", "http", true},
		{"http-prefixed name", "http-port", "", true},
		{"bare http name", "http", "", true},
		{"grpc name", "grpc-port", "", false},
		{"http name but another appProtocol", "http-port", "grpc", false},
	} {
		entry := &MeshRenderedService{Name: "gateway", Selector: map[string]string{"app": "gateway"}, PortName: tc.portName, Protocol: "TCP", AppProtocol: tc.appProtocol}
		if got := servicePortSaysHTTP(entry); got != tc.ok {
			t.Fatalf("%s: servicePortSaysHTTP = %v, want %v", tc.name, got, tc.ok)
		}
	}
}
