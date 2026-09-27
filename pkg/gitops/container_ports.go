package gitops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// A service agent writes the Kubernetes manifests of its service, and decides
// there which port the pod listens on for each endpoint. The CLI allocates the
// Service port (core's network.DeployedEndpointPorts, handed to the agent in its
// network mappings); the pod's port is the agent's own: a go-grpc agent binds a
// named endpoint on the port it was handed, while a postgres agent keeps 5432
// behind whatever Service port it publishes. No proto field reports it, so the
// one authoritative record is the rendered tree: the Service port entry the
// agent published for an endpoint's in-cluster port, and that entry's
// targetPort — resolved through the selected pod templates when it is named.

// deploymentSpecKey and endpointPortsKey locate a service's declared container
// ports: spec.deployment.endpoint-ports, endpoint name to port.
const (
	deploymentSpecKey = "deployment"
	endpointPortsKey  = "endpoint-ports"
)

// declaredEndpointPorts reads spec.deployment.endpoint-ports: the container
// port a module declares for each of a service's endpoints, which its own
// manifests (a NetworkPolicy, say) are written against. Absent, it is nil.
func declaredEndpointPorts(service *resources.Service) (map[string]uint32, error) {
	deployment, present := service.Spec[deploymentSpecKey]
	if !present || deployment == nil {
		return nil, nil
	}
	encoded, err := yaml.Marshal(deployment)
	if err != nil {
		return nil, fmt.Errorf("service %s: encode spec.%s: %w", service.Name, deploymentSpecKey, err)
	}
	var spec struct {
		EndpointPorts map[string]int64 `yaml:"endpoint-ports"`
	}
	if err := yaml.Unmarshal(encoded, &spec); err != nil {
		return nil, fmt.Errorf("service %s: spec.%s.%s must map endpoint names to ports: %w", service.Name, deploymentSpecKey, endpointPortsKey, err)
	}
	if len(spec.EndpointPorts) == 0 {
		return nil, nil
	}
	declared := make(map[string]uint32, len(spec.EndpointPorts))
	for endpoint, port := range spec.EndpointPorts {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("service %s: spec.%s.%s.%s = %d is not a TCP port", service.Name, deploymentSpecKey, endpointPortsKey, endpoint, port)
		}
		declared[endpoint] = uint32(port)
	}
	return declared, nil
}

// RenderedContainerPorts reads, from one rendered service unit, the container
// (pod) port each endpoint's traffic reaches, keyed by endpoint name.
// inCluster is each endpoint's in-cluster port — the Service port its agent was
// handed (orchestration.InClusterPorts). The unit's environment overlay is
// built with Kustomize, so an overlay patching a port is honoured; for each
// endpoint, every Service port entry publishing its in-cluster port contributes
// its targetPort (the port itself when absent, a container port looked up by
// name in the pods the Service selects when named). An endpoint no Service
// publishes is absent; entries that disagree are an error.
func RenderedContainerPorts(unitDir, environment string, inCluster map[string]uint32) (map[string]uint32, error) {
	overlay := filepath.Join(unitDir, "overlays", environment)
	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	built, err := kustomizer.Run(filesys.MakeFsOnDisk(), overlay)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", overlay, err)
	}
	output, err := built.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", overlay, err)
	}
	manifests, _, err := decodeYAML("kustomize:"+filepath.ToSlash(overlay), output)
	if err != nil {
		return nil, err
	}
	var services []renderedService
	var pods []renderedPod
	for _, item := range manifests {
		if item.group == "" && item.kind == "Service" {
			services = append(services, decodeRenderedService(item.value))
			continue
		}
		if pod, ok := decodeRenderedPod(item); ok {
			pods = append(pods, pod)
		}
	}
	endpoints := make([]string, 0, len(inCluster))
	for endpoint := range inCluster {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)
	ports := make(map[string]uint32, len(inCluster))
	for _, endpoint := range endpoints {
		port := inCluster[endpoint]
		targets := map[uint32][]string{}
		for i := range services {
			service := &services[i]
			for _, entry := range service.ports {
				if entry.port != port {
					continue
				}
				target, resolveErr := service.resolveTarget(entry, pods)
				if resolveErr != nil {
					return nil, fmt.Errorf("endpoint %s: %w", endpoint, resolveErr)
				}
				targets[target] = append(targets[target], service.name)
			}
		}
		switch len(targets) {
		case 0:
			continue
		case 1:
			for target := range targets {
				ports[endpoint] = target
			}
		default:
			var found []string
			for target, names := range targets {
				found = append(found, fmt.Sprintf("%d (Service %s)", target, strings.Join(names, ", ")))
			}
			sort.Strings(found)
			return nil, fmt.Errorf("endpoint %s: Services publishing port %d target different container ports: %s", endpoint, port, strings.Join(found, "; "))
		}
	}
	return ports, nil
}

// verifyDeclaredEndpointPorts refuses a service whose declared
// spec.deployment.endpoint-ports disagrees with the unit its agent rendered: a
// declaration naming an endpoint that is not rendered in-cluster, one whose
// in-cluster port no rendered Service publishes, and one whose value differs
// from the container port that Service targets. A service with no declaration
// passes without its tree being read. Every violation is reported at once.
func verifyDeclaredEndpointPorts(unitDir, environment, module string, service *resources.Service, inCluster map[string]uint32) error {
	declared, err := declaredEndpointPorts(service)
	if err != nil || len(declared) == 0 {
		return err
	}
	unique := resources.ServiceUnique(module, service.Name)
	container, err := RenderedContainerPorts(unitDir, environment, inCluster)
	if err != nil {
		return fmt.Errorf("service %s: read the container ports its agent rendered: %w", unique, err)
	}
	endpoints := make([]string, 0, len(declared))
	for endpoint := range declared {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)
	var violations []string
	for _, endpoint := range endpoints {
		want := declared[endpoint]
		servicePort, placed := inCluster[endpoint]
		if !placed {
			violations = append(violations, fmt.Sprintf(
				"%s = %d names an endpoint that is not rendered in-cluster in environment %s (in-cluster endpoints: %s)",
				endpoint, want, environment, inClusterEndpointList(inCluster)))
			continue
		}
		actual, published := container[endpoint]
		if !published {
			violations = append(violations, fmt.Sprintf(
				"%s = %d, but no rendered Service publishes its in-cluster port %d",
				endpoint, want, servicePort))
			continue
		}
		if actual != want {
			violations = append(violations, fmt.Sprintf(
				"%s = %d, but its pods listen on container port %d (the Service publishes port %d with targetPort %d)",
				endpoint, want, actual, servicePort, actual))
		}
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("service %s: spec.%s.%s disagrees with the manifests its agent rendered for environment %s:\n  %s",
		unique, deploymentSpecKey, endpointPortsKey, environment, strings.Join(violations, "\n  "))
}

// verifyGraphDeclaredEndpointPorts runs verifyDeclaredEndpointPorts over every
// service a single-service render staged under modules/<module>/services/<name>.
// A managed service renders no workload, so it has no container port to agree
// with and is skipped, as is a service the flow staged no unit for.
func verifyGraphDeclaredEndpointPorts(stage string, env *environments.Environment, graph map[string]*resources.Service, inClusterPorts map[string]map[string]uint32) error {
	uniques := make([]string, 0, len(graph))
	for unique := range graph {
		uniques = append(uniques, unique)
	}
	sort.Strings(uniques)
	for _, unique := range uniques {
		service := graph[unique]
		identity, err := service.Identity()
		if err != nil {
			return fmt.Errorf("service %s: %w", unique, err)
		}
		if _, managed := env.ManagedService(identity.Module, identity.Name); managed {
			continue
		}
		unitDir := filepath.Join(stage, "modules", identity.Module, serviceUnitDir, identity.Name)
		if _, statErr := os.Stat(unitDir); errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if err := verifyDeclaredEndpointPorts(unitDir, env.Name, identity.Module, service, inClusterPorts[unique]); err != nil {
			return err
		}
	}
	return nil
}

func inClusterEndpointList(inCluster map[string]uint32) string {
	if len(inCluster) == 0 {
		return "none"
	}
	endpoints := make([]string, 0, len(inCluster))
	for endpoint := range inCluster {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)
	return strings.Join(endpoints, ", ")
}

// ServiceContainerPorts reads the container ports of one service from the
// module render committed at <workspace>/deployments/modules/<module>. rendered
// is that render's directory, empty when the module has no render or the render
// carries no unit for the service (a managed service renders none). A render for
// another environment is an error: its ports would silently be the wrong ones.
func ServiceContainerPorts(workspace *resources.Workspace, module, service, environment string, inCluster map[string]uint32) (ports map[string]uint32, rendered string, err error) {
	root := moduleRenderDestination(workspace, module)
	inventory, err := LoadInventory(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if inventory.Environment != environment {
		return nil, "", fmt.Errorf("the render at %s is for environment %s, not %s: render %s for %s, or pass --env %s",
			root, inventory.Environment, environment, module, environment, inventory.Environment)
	}
	for _, unit := range inventory.Units {
		if unit.Kind != UnitKindService || unit.Name != service || unit.Path == "" || unit.Managed {
			continue
		}
		ports, err = RenderedContainerPorts(filepath.Join(root, filepath.FromSlash(unit.Path)), environment, inCluster)
		if err != nil {
			return nil, "", fmt.Errorf("service %s/%s: %w", module, service, err)
		}
		return ports, root, nil
	}
	return nil, "", nil
}

// renderedService is one Kubernetes Service of a rendered unit.
type renderedService struct {
	name     string
	selector map[string]string
	ports    []renderedServicePort
}

// renderedServicePort is one spec.ports entry. targetPort is either a number
// or the name of a container port; neither set means the port itself.
type renderedServicePort struct {
	port       uint32
	targetPort uint32
	targetName string
}

// renderedPod is the pod template of one workload (or a bare Pod).
type renderedPod struct {
	workload string
	labels   map[string]string
	// ports is every named container port, by name; a name declared twice
	// holds both values and resolves to neither.
	ports map[string][]uint32
}

func (service *renderedService) resolveTarget(entry renderedServicePort, pods []renderedPod) (uint32, error) {
	switch {
	case entry.targetName != "":
	case entry.targetPort != 0:
		return entry.targetPort, nil
	default:
		return entry.port, nil
	}
	if len(service.selector) == 0 {
		return 0, fmt.Errorf("rendered Service %s port %d targets container port %q by name but selects no pods", service.name, entry.port, entry.targetName)
	}
	found := map[uint32][]string{}
	for _, pod := range pods {
		if !selects(service.selector, pod.labels) {
			continue
		}
		for _, port := range pod.ports[entry.targetName] {
			found[port] = append(found[port], pod.workload)
		}
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("rendered Service %s port %d targets container port %q, which no pod it selects declares", service.name, entry.port, entry.targetName)
	case 1:
		for port := range found {
			return port, nil
		}
	}
	var described []string
	for port, workloads := range found {
		described = append(described, fmt.Sprintf("%d (%s)", port, strings.Join(workloads, ", ")))
	}
	sort.Strings(described)
	return 0, fmt.Errorf("rendered Service %s port %d targets container port %q, which the pods it selects declare as different ports: %s",
		service.name, entry.port, entry.targetName, strings.Join(described, "; "))
}

func selects(selector, labels map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func decodeRenderedService(value map[string]any) renderedService {
	service := renderedService{name: metadataString(value, "name"), selector: map[string]string{}}
	spec, _ := value["spec"].(map[string]any)
	if selector, ok := spec["selector"].(map[string]any); ok {
		for key, label := range selector {
			service.selector[key] = fmt.Sprint(label)
		}
	}
	entries, _ := spec["ports"].([]any)
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		port, _ := portNumber(entry["port"])
		decoded := renderedServicePort{port: port}
		if target, numeric := portNumber(entry["targetPort"]); numeric {
			decoded.targetPort = target
		} else if name, named := entry["targetPort"].(string); named {
			decoded.targetName = name
		}
		service.ports = append(service.ports, decoded)
	}
	return service
}

// podTemplateKinds are the workloads whose spec.template is a pod template.
var podTemplateKinds = map[string]bool{
	"apps/Deployment": true, "apps/StatefulSet": true, "apps/DaemonSet": true,
	"apps/ReplicaSet": true, "batch/Job": true, "/ReplicationController": true,
}

func decodeRenderedPod(item manifest) (renderedPod, bool) {
	var metadata, spec map[string]any
	switch {
	case item.group == "" && item.kind == "Pod":
		metadata, _ = item.value["metadata"].(map[string]any)
		spec, _ = item.value["spec"].(map[string]any)
	case podTemplateKinds[item.group+"/"+item.kind]:
		workloadSpec, _ := item.value["spec"].(map[string]any)
		template, _ := workloadSpec["template"].(map[string]any)
		metadata, _ = template["metadata"].(map[string]any)
		spec, _ = template["spec"].(map[string]any)
	default:
		return renderedPod{}, false
	}
	pod := renderedPod{
		workload: item.kind + "/" + metadataString(item.value, "name"),
		labels:   map[string]string{},
		ports:    map[string][]uint32{},
	}
	if labels, ok := metadata["labels"].(map[string]any); ok {
		for key, label := range labels {
			pod.labels[key] = fmt.Sprint(label)
		}
	}
	containers, _ := spec["containers"].([]any)
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		ports, _ := container["ports"].([]any)
		for _, rawPort := range ports {
			port, _ := rawPort.(map[string]any)
			name, _ := port["name"].(string)
			number, numeric := portNumber(port["containerPort"])
			if name != "" && numeric {
				pod.ports[name] = append(pod.ports[name], number)
			}
		}
	}
	return pod, true
}

// portNumber reads a decoded YAML port number.
func portNumber(value any) (uint32, bool) {
	var number int64
	switch typed := value.(type) {
	case int:
		number = int64(typed)
	case int64:
		number = typed
	case uint64:
		if typed > 65535 {
			return 0, false
		}
		number = int64(typed)
	case float64:
		number = int64(typed)
		if float64(number) != typed {
			return 0, false
		}
	default:
		return 0, false
	}
	if number < 1 || number > 65535 {
		return 0, false
	}
	return uint32(number), true
}
