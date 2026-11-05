package gitops

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/core/standards"
)

// bindGatewayServiceEvidence joins the logical gateway edges to the Services
// the agent actually rendered, including all environment overlay patches.
//
// The gateway's mesh endpoints are its HTTP presenter path (`rest`), the
// workload endpoint every composed workload mints on, and — when the
// environment sends every prepared runnable call to one of the gateway's own
// endpoints (`runnable-call-endpoint: <gateway>/<endpoint>`, the hop) — that
// endpoint, which the executors reach by the composition's own statement.
// callEndpoint is that declaration, "" when the environment makes none. Any
// other endpoint of the gateway with a mesh edge is a listener around the
// presenter path and is refused.
func bindGatewayServiceEvidence(record *MeshEdges, tree *renderRoot, stage, environment string, units []InventoryUnit, callEndpoint string) error {
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
	admitted := gatewayMeshEndpoints(record.Gateway, callEndpoint)
	seen := map[string]uint32{}
	for _, edge := range gateway {
		if !slices.Contains(admitted, edge.Endpoint) {
			return fmt.Errorf("gateway mesh endpoint %s cannot bypass its HTTP presenter path: the gateway's mesh endpoints are %s", edge.Endpoint, strings.Join(admitted, ", "))
		}
		if _, duplicate := seen[edge.Endpoint]; duplicate {
			return fmt.Errorf("gateway repeats endpoint %s", edge.Endpoint)
		}
		seen[edge.Endpoint] = edge.TargetPort
		if err := bindRenderedServicePort(edge, manifests); err != nil {
			return err
		}
	}
	if seen["rest"] == 0 || seen[gatewayWorkloadEndpoint] == 0 {
		return fmt.Errorf("gateway rest and workload must both resolve a container port")
	}
	byPort := map[uint32]string{}
	for _, endpoint := range admitted {
		port, bound := seen[endpoint]
		if !bound {
			continue
		}
		if other, shared := byPort[port]; shared {
			return fmt.Errorf("gateway endpoints %s and %s must have distinct resolved container ports, both resolve %d", other, endpoint, port)
		}
		byPort[port] = endpoint
	}
	return nil
}

// gatewayMeshEndpoints is the gateway's admitted mesh endpoints: rest, the
// workload endpoint, and the endpoint of the environment's declared runnable
// call endpoint when it is the gateway's own. The declaration is
// `<module>/<service>/<endpoint>`; one naming another service admits nothing
// here, since that service's edges are judged where they are rendered.
func gatewayMeshEndpoints(gateway, callEndpoint string) []string {
	admitted := []string{"rest", gatewayWorkloadEndpoint}
	if cut := strings.LastIndex(callEndpoint, "/"); cut > 0 {
		unique, endpoint := callEndpoint[:cut], callEndpoint[cut+1:]
		if unique == gateway && endpoint != "" && !slices.Contains(admitted, endpoint) {
			admitted = append(admitted, endpoint)
		}
	}
	return admitted
}

// gatewayOverlay is the environment overlay of the gateway's one rendered
// service unit: the Services the edges are checked against.
func gatewayOverlay(gateway, stage, environment string, units []InventoryUnit) (string, error) {
	_, service, _ := strings.Cut(gateway, "/")
	directory := ""
	for idx := range units {
		unit := &units[idx]
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
