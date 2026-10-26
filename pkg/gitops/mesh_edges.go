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
// `meshEdges`), documented in docs/module-manifests.md and versioned by
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
//     than named;
//   - the endpoint a module's DERIVED OPERATION is called on (the owner's
//     served connect endpoint, or the published REST one) admits every
//     service that installs prepared runnable bindings — the services
//     declaring the `runnable-bindings` workspace-configuration group. An
//     executor (the runtime) calls owners it does not and must not declare;
//     the composition prepares those calls (`codefly generate
//     runnable-bindings`), so the composition is where the edge comes from.
//     Without it the owner's connect endpoint had no consumer at all and every
//     fan-out operation on a cell settled OWNER_UNAVAILABLE at the mesh.
//
// One source is not in the composition at all: a service whose render includes
// a workload that does not serve its endpoints — an agent's bootstrap Job,
// which runs under the service's own ServiceAccount and therefore under the
// service's own mesh principal — reaches the endpoints of the very service it
// belongs to. Nothing declares that, because it is not a dependency between
// services; it is one rendered workload reaching another. The render is the
// only place it is visible, so a service that renders one admits itself
// (selfConsuming below). Without it the Postgres bootstrap Job is denied by
// the cell's default-deny mesh on the database it exists to initialize.
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
	// principal, the one its module manifest names.
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
	// executors are the principals of the services that install prepared
	// runnable bindings; owners are the endpoints their prepared calls target.
	executors []string
	reach     *runnableReach
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
	// Every composed workload's principal, by module: a managed or an
	// excluded service has no workload of its own in the cell, so it holds no
	// principal.
	for _, ref := range refs {
		if _, managed := env.ManagedService(ref.Module, ref.Name); managed || env.Excludes(ref.Module, ref.Name) {
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
	// The reach the composition prepares rather than declares, derived once
	// for the cell file and the mesh alike (derivedRunnableReach): the
	// executors' principals, and the owner endpoints their prepared bindings
	// call.
	reach, err := derivedRunnableReach(ctx, workspace)
	if err != nil {
		return nil, err
	}
	index.reach = reach
	for _, executor := range reach.executors {
		if _, managed := env.ManagedService(executor.Module, executor.Service); managed {
			continue
		}
		namespace := env.ModuleNamespace(workspace, executor.Module)
		if namespace == "" {
			continue
		}
		index.executors = append(index.executors, meshPrincipal(host, namespace, executor.Service))
	}
	slices.Sort(index.executors)
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
	if index.reach.reaches(module, service, endpoint) {
		for _, principal := range index.executors {
			admitted[principal] = true
		}
	}
	return slices.Sorted(maps.Keys(admitted))
}

// renderTreeSelfConsumes reads, off one unit's rendered overlay, whether the
// service reaches its own endpoints: it does when the overlay holds a workload
// that does not serve them — an agent's bootstrap Job, running under the
// service's own ServiceAccount and so under its own mesh principal.
//
// It is read from the render rather than the composition because the
// composition never says it: a Job the agent emits is not a dependency between
// services, and nothing but the rendered tree knows it exists.
func renderTreeSelfConsumes(tree *renderRoot, unitDir, environment string) (bool, error) {
	workloads, err := renderedWorkloadsInTree(tree, unitDir, environment)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(workloads, func(workload renderedWorkload) bool {
		return !servingKind(workload.Kind)
	}), nil
}

// deriveMeshEdges computes the edges into one module's services, from the
// composition the render is of and the in-cluster ports it learned. Nothing
// is derived for an environment that declares no host: there is no mesh to
// deliver edges to.
//
// selfConsuming names, by service unique, the services whose rendered tree
// holds a workload that does not serve the service's endpoints and so reaches
// them as a client under the service's own principal.
func deriveMeshEdges(
	ctx context.Context,
	workspace *resources.Workspace,
	env *environments.Environment,
	module *resources.Module,
	services []*resources.Service,
	ports, containerPorts map[string]map[string]uint32,
	selfConsuming map[string]bool,
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
			principal := meshPrincipal(env.Host, namespace, service.Name)
			principals := index.sources(module.Name, service.Name, endpoint.Name)
			// A non-serving workload of this service runs as this service and
			// reaches this endpoint; the composition never declares it, so the
			// render is where it enters.
			if selfConsuming[unique] && !slices.Contains(principals, principal) {
				principals = append(principals, principal)
				slices.Sort(principals)
			}
			// The cell's ingress gateway reaches exactly the endpoints the
			// environment routes (`ingress:`): the composition decides its
			// entrance — a product's portal, its marketing site, or the
			// host gateway — and an endpoint routed at the edge must be one
			// its module declares public, judged on the manifest endpoint
			// as core's IsExposedEndpoint judges the proto one. A public
			// endpoint nobody routes has no ingress edge.
			ingress := routedAtTheEdge(env, unique, endpoint.Name)
			if ingress && endpoint.Exposure != resources.ExposurePublic {
				return nil, fmt.Errorf("the environment routes %s/%s at its edge, but %s declares it %q, not public; a route reaches only an endpoint its module exposes", unique, endpoint.Name, service.Name, endpoint.Exposure)
			}
			if len(principals) == 0 && !ingress {
				continue
			}
			targetPort := containerPorts[unique][endpoint.Name]
			if targetPort == 0 {
				return nil, fmt.Errorf("mesh endpoint %s/%s has no resolved container target port", unique, endpoint.Name)
			}
			record.Edges = append(record.Edges, MeshEdge{
				Module: module.Name, Service: service.Name, Namespace: namespace,
				Account: workloadAccount(service.Name), Principal: principal,
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

// routedAtTheEdge reports whether the environment declares an ingress route
// onto one endpoint of one service, named module-qualified as a route names it.
func routedAtTheEdge(env *environments.Environment, unique, endpoint string) bool {
	for _, route := range env.Ingress {
		if route.Service == unique && route.Endpoint == endpoint {
			return true
		}
	}
	return false
}
