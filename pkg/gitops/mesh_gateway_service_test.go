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
	return bindGatewayServiceEvidence(record, tree, unit, "staging", []InventoryUnit{{Kind: UnitKindService, Name: "gateway", Path: "."}})
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
