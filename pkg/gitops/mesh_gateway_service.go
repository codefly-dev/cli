package gitops

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/standards"
)

// bindGatewayServiceEvidence joins the logical gateway edges to the Services
// the agent actually rendered, including all environment overlay patches.
func bindGatewayServiceEvidence(record *MeshEdges, tree *renderRoot, stage, environment string, units []InventoryUnit) error {
	if record == nil {
		return nil
	}
	var gateway []*MeshEdge
	for i := range record.Edges {
		edge := &record.Edges[i]
		if edge.Module+"/"+edge.Service == record.Gateway {
			gateway = append(gateway, edge)
		}
	}
	if len(gateway) == 0 {
		return nil // This module's tree does not own the gateway.
	}
	directory, err := gatewayOverlay(record.Gateway, stage, environment, units)
	if err != nil {
		return err
	}
	manifests, err := overlayManifestsInTree(tree, directory)
	if err != nil {
		return err
	}
	seen := map[string]uint32{}
	for _, edge := range gateway {
		if edge.Endpoint != "rest" && edge.Endpoint != gatewayWorkloadEndpoint {
			return fmt.Errorf("gateway mesh endpoint %s cannot bypass its HTTP presenter path", edge.Endpoint)
		}
		if _, duplicate := seen[edge.Endpoint]; duplicate {
			return fmt.Errorf("gateway repeats endpoint %s", edge.Endpoint)
		}
		seen[edge.Endpoint] = edge.TargetPort
		if err := bindRenderedServicePort(edge, manifests); err != nil {
			return err
		}
	}
	if len(seen) != 2 || seen["rest"] == 0 || seen[gatewayWorkloadEndpoint] == 0 || seen["rest"] == seen[gatewayWorkloadEndpoint] {
		return fmt.Errorf("gateway rest and workload must have distinct resolved container ports")
	}
	return nil
}

// gatewayOverlay is the environment overlay of the gateway's one rendered
// service unit: the Services the edges are checked against.
func gatewayOverlay(gateway, stage, environment string, units []InventoryUnit) (string, error) {
	_, service, _ := strings.Cut(gateway, "/")
	directory := ""
	for _, unit := range units {
		if unit.Kind == UnitKindService && unit.Name == service && !unit.Managed && unit.Path != "" {
			if directory != "" {
				return "", fmt.Errorf("gateway %s has more than one rendered service unit", gateway)
			}
			directory = filepath.Join(stage, unit.Path, "overlays", environment)
		}
	}
	if directory == "" {
		return "", fmt.Errorf("gateway %s has no rendered service unit", gateway)
	}
	return directory, nil
}

// bindRenderedServicePort finds the one rendered Service port publishing the
// edge's Service port and pins it on the edge: a named HTTP port on a Service
// with a pod selector, in the edge's namespace.
func bindRenderedServicePort(edge *MeshEdge, manifests []manifest) error {
	for _, item := range manifests {
		if item.group != "" || item.kind != "Service" {
			continue
		}
		spec, _ := item.value["spec"].(map[string]any)
		ports, _ := spec["ports"].([]any)
		for _, raw := range ports {
			port, _ := raw.(map[string]any)
			actual, numeric := portNumber(port["port"])
			if !numeric || actual != edge.Port {
				continue
			}
			if edge.RenderedService != nil {
				return fmt.Errorf("gateway endpoint %s is published by more than one Service port", edge.Endpoint)
			}
			if namespace := metadataString(item.value, "namespace"); namespace != "" && namespace != edge.Namespace {
				return fmt.Errorf("gateway endpoint %s Service is in a different namespace", edge.Endpoint)
			}
			entry, err := meshServicePortEvidence(edge.Endpoint, item, spec, port)
			if err != nil {
				return err
			}
			edge.RenderedService = entry
		}
	}
	if edge.RenderedService == nil {
		return fmt.Errorf("gateway endpoint %s has no rendered Service port", edge.Endpoint)
	}
	return nil
}

// meshServicePortEvidence reads one Service port as the mesh evidence names it.
func meshServicePortEvidence(endpoint string, item manifest, spec, port map[string]any) (*MeshRenderedService, error) {
	entry := &MeshRenderedService{Name: metadataString(item.value, "name"), Selector: map[string]string{}}
	entry.PortName, _ = port["name"].(string)
	entry.Protocol, _ = port["protocol"].(string)
	if _, present := port["protocol"]; !present {
		entry.Protocol = "TCP" // Kubernetes ServicePort default.
	}
	entry.AppProtocol, _ = port["appProtocol"].(string)
	selector, _ := spec["selector"].(map[string]any)
	for key, rawLabel := range selector {
		label, ok := rawLabel.(string)
		if !ok || key == "" || label == "" {
			return nil, fmt.Errorf("gateway endpoint %s has an invalid Service selector", endpoint)
		}
		entry.Selector[key] = label
	}
	// The port must say it carries HTTP, the API the presenter waypoint
	// forwards: `appProtocol: http` is the explicit spelling, and a port
	// named `http` or `http-<suffix>` is the protocol-selection convention
	// the mesh honours and the service agents render today. Nothing else is
	// a gateway port.
	if entry.Name == "" || len(entry.Selector) == 0 || entry.PortName == "" || entry.Protocol != "TCP" || !servicePortSaysHTTP(entry) {
		return nil, fmt.Errorf("gateway endpoint %s requires a named HTTP Service port with a pod selector (appProtocol: http, or a port named http or http-<suffix>)", endpoint)
	}
	return entry, nil
}

// servicePortSaysHTTP reports whether a Service port declares HTTP: by
// appProtocol, or by the mesh's port-name protocol selection (`http`,
// `http-<suffix>`).
func servicePortSaysHTTP(entry *MeshRenderedService) bool {
	if entry.AppProtocol != "" {
		return entry.AppProtocol == standards.HTTP
	}
	return entry.PortName == standards.HTTP || strings.HasPrefix(entry.PortName, standards.HTTP+"-")
}
