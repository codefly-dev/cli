package gitops

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// MESH EDGES: THE L4 REACHABILITY A RENDER DELIVERS.
//
// Inside a cell every hop is plain HTTP and the mesh carries transport
// security and destination identity: a workload's principal is its
// ServiceAccount's SPIFFE ID, and whether one workload may reach another's
// port is a mesh policy the cell's protected applier compiles into
// default-deny AuthorizationPolicy objects. The CLI never writes one of those.
// What it knows — and nothing else in the cell does — is every edge: which
// endpoint each service declares, on which port, and who the composition
// declares reaches it. So with the rendered tree it delivers those edges as
// data, in the render record at the tree root (.codefly-render.json,
// `meshEdges`), documented in docs/solution-host-bindings.md and versioned by
// MeshEdgesSchemaVersion. Each module's tree carries the edges INTO that
// module's workloads; the applier reads every tree it applies.
//
// The sources are the composition's own declarations, derived once each:
//
//   - a module endpoint admits the modules whose declarers ask for it
//     (core's Workspace.DeriveAllowModules, the one derivation of the
//     allow-list, at module granularity: every workload of a consumer module)
//     and, when composed solutions consume it through the host, the gateway's
//     principal (orchestration.HostRouteTargets, the same derivation that
//     hands the gateway its route carriers);
//   - a solution's backend endpoint admits the gateway's principal (the same
//     targets: the gateway fronts the solution);
//   - the gateway's `workload` endpoint admits every composed workload's
//     principal (every module mints on it);
//   - a public endpoint — the gateway's `rest` — admits the cell's ingress
//     gateway, whose principal is the cell's to name, so it is marked rather
//     than named.
//
// An endpoint nobody is declared to reach has no edge: default deny is what
// the applier compiles, and the absence is the declaration.

// MeshEdgesSchemaVersion versions the `meshEdges` record. A reader refuses a
// version it does not know rather than compile edges it misreads.
const MeshEdgesSchemaVersion = 1

// gatewayWorkloadEndpoint is the name of the gateway's endpoint every composed
// workload mints on, as the host declares it (`workload`, api http, internal).
const gatewayWorkloadEndpoint = "workload"

// MeshEdges is the record one rendered tree carries: the edges into the
// module's workloads, every principal under one trust domain.
type MeshEdges struct {
	SchemaVersion int `json:"schemaVersion"`
	// TrustDomain is the mesh's SPIFFE trust domain every principal below is
	// under: the environment's host.trust_domain.
	TrustDomain string `json:"trustDomain"`
	// Gateway is the composition's explicit logical gateway reference.
	Gateway string `json:"gateway"`
	// Edges are the endpoints reached, with who reaches them. Sorted by
	// service then endpoint; each once.
	Edges []MeshEdge `json:"edges"`
}

// MeshEdge is one endpoint of one workload and the principals admitted to it.
type MeshEdge struct {
	// Module, Service and Namespace name the destination workload; Account is
	// the ServiceAccount it runs as and Principal that account's mesh
	// principal, the one its presence names.
	Module    string `json:"module"`
	Service   string `json:"service"`
	Namespace string `json:"namespace"`
	Account   string `json:"account"`
	Principal string `json:"principal"`
	// Endpoint and Port are the destination's declared endpoint and its
	// in-cluster port, the one the render hands the service's agent to
	// publish as its Kubernetes Service port.
	Endpoint string `json:"endpoint"`
	Port     uint32 `json:"port"`
	// TargetPort is the container port resolved from the effective Service.
	// L4 policy matches this port; Service-attached policy matches Port.
	TargetPort uint32 `json:"targetPort"`
	// RenderedService pins the actual HTTP Service traversed by the gateway's
	// presenter waypoint. It is populated from the effective render, not names.
	RenderedService *MeshRenderedService `json:"renderedService,omitempty"`
	// Principals are the mesh principals admitted to the port, sorted, each
	// once. Empty only when Ingress is set.
	Principals []string `json:"principals,omitempty"`
	// Ingress marks a port the cell's ingress gateway is admitted to: a
	// public endpoint, reached by the cell's own edge whose principal the
	// cell names.
	Ingress bool `json:"ingress,omitempty"`
}

// MeshRenderedService is the Service evidence the protected mesh owner checks
// against its inventory before admitting a presenter path.
type MeshRenderedService struct {
	Name        string            `json:"name"`
	Selector    map[string]string `json:"selector"`
	PortName    string            `json:"portName"`
	Protocol    string            `json:"protocol"`
	AppProtocol string            `json:"appProtocol"`
}

// meshComposition is what the composition says about every workload: the
// principals by module, every principal, the gateway's, and the endpoints
// the gateway forwards to.
type meshComposition struct {
	byModule         map[string][]string
	every            []string
	gateway          string
	gatewayPrincipal string
	allow            resources.DerivedAllowModules
	targets          []orchestration.HostRouteTarget
}

// indexMeshComposition derives the composition's side of every edge once.
func indexMeshComposition(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (*meshComposition, error) {
	host := env.Host
	allow, err := workspace.DeriveAllowModules(ctx)
	if err != nil {
		return nil, err
	}
	gatewayModule, gatewayService, _ := strings.Cut(host.Gateway, "/")
	targets, err := orchestration.HostRouteTargets(ctx, workspace, gatewayModule)
	if err != nil {
		return nil, err
	}
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return nil, err
	}
	index := &meshComposition{byModule: map[string][]string{}, gateway: host.Gateway, allow: allow, targets: targets}
	// Every composed workload's principal, by module: a managed service has
	// no workload of its own in the cell, so it holds no principal.
	for _, ref := range refs {
		if _, managed := env.ManagedService(ref.Module, ref.Name); managed {
			continue
		}
		namespace := env.ModuleNamespace(workspace, ref.Module)
		if namespace == "" {
			return nil, fmt.Errorf("%s/%s has no resolved namespace, so its mesh principal cannot be named", ref.Module, ref.Name)
		}
		principal := meshPrincipal(host, namespace, ref.Name)
		index.byModule[ref.Module] = append(index.byModule[ref.Module], principal)
		index.every = append(index.every, principal)
	}
	if host.Gateway != "" {
		index.gatewayPrincipal = meshPrincipal(host, env.ModuleNamespace(workspace, gatewayModule), gatewayService)
	}
	return index, nil
}

// sources is who the composition admits to one endpoint, sorted and each
// once: the consumer modules' workloads, the gateway when it forwards to the
// endpoint, and every workload on the gateway's workload port.
func (index *meshComposition) sources(module, service, endpoint string) []string {
	admitted := map[string]bool{}
	if list, declared := index.allow.Of(module, service, endpoint); declared {
		for _, consumer := range list.Modules {
			for _, principal := range index.byModule[consumer] {
				admitted[principal] = true
			}
		}
	}
	for _, target := range index.targets {
		if target.Module == module && target.Service == service && target.Endpoint == endpoint && index.gatewayPrincipal != "" {
			admitted[index.gatewayPrincipal] = true
		}
	}
	if resources.ServiceUnique(module, service) == index.gateway && endpoint == gatewayWorkloadEndpoint {
		for _, principal := range index.every {
			admitted[principal] = true
		}
	}
	return slices.Sorted(maps.Keys(admitted))
}

// deriveMeshEdges computes the edges into one module's services, from the
// composition the render is of and the in-cluster ports it learned. Nothing
// is derived for an environment that declares no host: there is no mesh to
// deliver edges to.
func deriveMeshEdges(
	ctx context.Context,
	workspace *resources.Workspace,
	env *environments.Environment,
	module *resources.Module,
	services []*resources.Service,
	ports, containerPorts map[string]map[string]uint32,
) (*MeshEdges, error) {
	if env == nil || env.Host == nil {
		return nil, nil
	}
	index, err := indexMeshComposition(ctx, workspace, env)
	if err != nil {
		return nil, fmt.Errorf("derive the mesh edges of %s: %w", module.Name, err)
	}
	namespace := env.ModuleNamespace(workspace, module.Name)
	record := &MeshEdges{SchemaVersion: MeshEdgesSchemaVersion, TrustDomain: env.Host.TrustDomain, Gateway: env.Host.Gateway}
	for _, service := range services {
		if service == nil {
			continue
		}
		if _, managed := env.ManagedService(module.Name, service.Name); managed {
			continue
		}
		unique := resources.ServiceUnique(module.Name, service.Name)
		for _, endpoint := range service.Endpoints {
			if endpoint == nil {
				continue
			}
			port := ports[unique][endpoint.Name]
			if port == 0 {
				// An external endpoint, resolved to a public host: not a port
				// in the cell.
				continue
			}
			principals := index.sources(module.Name, service.Name, endpoint.Name)
			// Public exposure, judged on the manifest endpoint as core's
			// IsExposedEndpoint judges the proto one: the one condition a
			// public address exists on, and so the one the cell's ingress
			// gateway reaches.
			ingress := endpoint.Exposure == resources.ExposurePublic
			if len(principals) == 0 && !ingress {
				continue
			}
			targetPort := containerPorts[unique][endpoint.Name]
			if targetPort == 0 {
				return nil, fmt.Errorf("mesh endpoint %s/%s has no resolved container target port", unique, endpoint.Name)
			}
			if ingress && (unique != index.gateway || endpoint.Name != "rest") {
				return nil, fmt.Errorf("mesh ingress may reach only the host gateway's rest endpoint, not %s/%s", unique, endpoint.Name)
			}
			record.Edges = append(record.Edges, MeshEdge{
				Module: module.Name, Service: service.Name, Namespace: namespace,
				Account: workloadAccount(service.Name), Principal: meshPrincipal(env.Host, namespace, service.Name),
				Endpoint: endpoint.Name, Port: port, TargetPort: targetPort, Principals: principals, Ingress: ingress,
			})
		}
	}
	sort.Slice(record.Edges, func(i, j int) bool {
		if record.Edges[i].Service != record.Edges[j].Service {
			return record.Edges[i].Service < record.Edges[j].Service
		}
		return record.Edges[i].Endpoint < record.Edges[j].Endpoint
	})
	return record, nil
}
