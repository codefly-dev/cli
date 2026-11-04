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
// A MODULE SERVICE IS REACHABLE ONLY FROM ITS HOP. Where the composition
// stands a hop in front of its modules (hop.go derives it from the
// composition's own three statements: the modules the `hop` group names, the
// service declaring `hop-routes`, and the listeners that generated route table
// names), the sources above are not the last word on a fronted module's
// endpoints. The whole reason to stand a hop is that a module's own code never
// has to install the check, so its listeners must have no other way to receive
// traffic. On a non-local environment, then, each endpoint of a fronted
// module's service admits:
//
//   - the HOP's principal, and only it, on the listeners the hop's route table
//     names — the one statement of the hop's reach, since the hop declares no
//     service dependency. An endpoint the table does not name is not given it:
//     this narrows, it never widens, and a principal of the hop's own MODULE
//     is no exception. The allow-list is at module granularity, so a SIBLING
//     service of the hop asking for a module listener arrives as a principal
//     of the hop's module; it is refused by name, naming the declarer that
//     asked (the hop's module's own dependency declarers say which service
//     did), never collapsed into the hop's principal. Collapsing it both
//     widened the hop — onto listeners its route table does not name, where
//     the authority holds no policy table — and killed the sibling's declared
//     reach at runtime with nothing said at render time.
//   - the callers the SERVICE's manifest admits by name
//     (admitted_sources.go) AND the environment co-declares
//     (environments.EnvironmentAdmittedSource): a worker pulling from a
//     store, a webhook receiver. Never implied from a dependency graph — an
//     edge that admitted itself would admit exactly what this rule refuses —
//     and never on a listener the hop's route table names, which is an API
//     call through the hop by the composition's own statement. An admission
//     written in the module alone is a waiver of the permission layer granted
//     by the code it protects, so it takes the operator's signature too.
//   - itself, when its own render holds a non-serving workload (selfConsuming
//     below), which is one workload of the service reaching another.
//
// Every other declared reach is a route AROUND the hop — a dependency on a
// module service, a host route onto a module listener, prepared runnable
// bindings sent to an owner rather than through the hop — and the render is
// refused, naming the reach and what to change. Inside `local` the reach is
// recorded as declared: a developer's own cluster is where calling a module
// directly is the point, and the refusal is the deployed rule.
//
// An endpoint nobody is declared to reach has no edge: default deny is what
// the applier compiles, and the absence is the declaration.

// MeshEdgesSchemaVersion versions the `meshEdges` record. A reader refuses a
// version it does not know rather than compile edges it misreads.
//
// 2 adds `admissions`: every effective waiver of "only the hop", so an applier
// and a reviewer read the whole waiver set off the record instead of reading
// every module's manifests. The version is bumped rather than the field added
// quietly because the MEANING of the edges changed with it — a manifest
// admission now takes the environment's co-signature, so a version-1 record's
// principals could include a waiver no operator accepted.
const MeshEdgesSchemaVersion = 2

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
	// Admissions is every waiver of "only the hop" this render delivered:
	// each caller a service's manifest proposed and the environment
	// co-declared, by endpoint. The edges above carry the resulting
	// principals; this says which of them is a waiver rather than the hop,
	// so the whole waiver set is readable without opening a manifest. Sorted
	// by service, endpoint then caller.
	Admissions []MeshAdmission `json:"admissions,omitempty"`
}

// MeshAdmission is one effective admission: a caller a module's own manifest
// admits to one of its endpoints and the environment co-declares, besides the
// hop. A co-declared admission this environment runs no workload for is
// recorded too, with Omitted saying so: a waiver that is not in force here is
// part of the waiver set a reviewer reads, and silence would make the record
// disagree with the declarations behind it.
type MeshAdmission struct {
	// Module, Service and Endpoint name the admitting endpoint.
	Module   string `json:"module"`
	Service  string `json:"service"`
	Endpoint string `json:"endpoint"`
	// Caller is the admitted `<module>/<service>`, and Principal its mesh
	// principal. Principal is empty exactly when Omitted is set.
	Caller    string `json:"caller"`
	Principal string `json:"principal,omitempty"`
	// Omitted says why this admission carries no principal in this
	// environment: the caller is a workload it does not run (managed here, or
	// withdrawn by exclude-services). Empty for an admission in force.
	Omitted string `json:"omitted,omitempty"`
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
// principals by module, every principal, the gateway's, the endpoints the
// gateway forwards to, and the hop every fronted module is reached through.
type meshComposition struct {
	// workspace is the composition every name below is resolved against.
	workspace *resources.Workspace
	// workloads are the services this environment runs a workload for: a
	// managed or excluded service is not one, so it holds no principal and
	// cannot be admitted to anything.
	workloads []*resources.ServiceWithModule
	// composed is every service the COMPOSITION carries, whether or not this
	// environment runs it. The two are not the same refusal: a name the
	// composition does not carry at all is a mistake wherever it renders, and
	// one it carries but this environment manages or excludes is an edge with
	// nothing to point at here.
	composed         []*resources.ServiceWithModule
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
	// callEndpoint is the `<module>/<service>/<endpoint>` the environment
	// declares every prepared runnable call is sent to instead of the owner's
	// listener (runnable-call-endpoint: the hop, in a composition that stands
	// one). Empty sends each call to its owner, and the executors are then
	// admitted to the owners' endpoints as before.
	callEndpoint string
	// hop is the composition's hop, nil where it stands none.
	hop *compositionHop
	// hopPrincipal is the hop service's own mesh principal, the one a fronted
	// module's endpoints admit.
	hopPrincipal string
	// hopModuleAskers are the dependency declarers of the HOP's own module:
	// how a principal of that module which is not the hop's own is named in a
	// refusal. The derived allow-list is at module granularity, so it cannot
	// say which service asked; these declarations can, and a refusal that
	// cannot be acted on is a refusal that gets worked around.
	hopModuleAskers []resources.DependencyDeclarer
}

// meshSource is one admitted principal and WHAT the composition declared it
// by, so a reach refused as a route around the hop can be acted on rather
// than looked for.
type meshSource struct {
	principal string
	declared  string
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
	index := &meshComposition{workspace: workspace, byModule: map[string][]string{}, gateway: host.Gateway, allow: allow, targets: targets}
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
		index.workloads = append(index.workloads, ref)
	}
	index.composed = refs
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
	index.callEndpoint = env.RunnableCallEndpoint
	// The composition's hop, and the one principal a fronted module's
	// endpoints admit. A hop the environment does not run is refused here: the
	// modules named behind it would have no door at all.
	if index.hop, err = derivedCompositionHop(ctx, workspace, env); err != nil {
		return nil, err
	}
	if index.hop != nil {
		if _, managed := env.ManagedService(index.hop.Module, index.hop.Service); managed || env.Excludes(index.hop.Module, index.hop.Service) {
			return nil, fmt.Errorf("%s stands the composition's hop for modules %s, but environment %s does not run it (managed or exclude-services), so those modules are published through nothing",
				index.hop.unique(), strings.Join(index.hop.Fronted, ", "), env.Name)
		}
		namespace := env.ModuleNamespace(workspace, index.hop.Module)
		if namespace == "" {
			return nil, fmt.Errorf("%s stands the composition's hop but its module has no resolved namespace, so its mesh principal cannot be named", index.hop.unique())
		}
		index.hopPrincipal = meshPrincipal(host, namespace, index.hop.Service)
		if index.hopModuleAskers, err = hopModuleDeclarers(ctx, workspace, index.hop.Module); err != nil {
			return nil, err
		}
	}
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

// sources is who the composition admits to one endpoint, sorted by principal
// and each once, with what declared it: the consumer modules' workloads, the
// gateway when it forwards to the endpoint, every workload on the gateway's
// workload port, and the executors of prepared runnable bindings — at the
// owner's endpoint, or at the one endpoint the environment declares every
// prepared call is sent to instead.
func (index *meshComposition) sources(module, service, endpoint string) []meshSource {
	unique := resources.ServiceUnique(module, service)
	admitted := map[string]string{}
	declare := func(principal, why string) {
		if principal == "" {
			return
		}
		if _, already := admitted[principal]; !already {
			admitted[principal] = why
		}
	}
	if list, declared := index.allow.Of(module, service, endpoint); declared {
		for _, consumer := range list.Modules {
			for _, principal := range index.byModule[consumer] {
				declare(principal, fmt.Sprintf("module %s declares a run-stage dependency, or a configuration reference, reaching it", consumer))
			}
		}
	}
	for _, target := range index.targets {
		if target.Module == module && target.Service == service && target.Endpoint == endpoint {
			declare(index.gatewayPrincipal, fmt.Sprintf("the host routes it through the gateway %s", index.gateway))
		}
	}
	if unique == index.gateway && endpoint == gatewayWorkloadEndpoint {
		for _, principal := range index.every {
			declare(principal, "every composed workload mints on the gateway's workload endpoint")
		}
	}
	if index.callsThrough(unique, endpoint) {
		for _, principal := range index.executors {
			declare(principal, "it installs prepared runnable bindings whose calls are sent here (codefly generate runnable-bindings)")
		}
	}
	sources := make([]meshSource, 0, len(admitted))
	for _, principal := range slices.Sorted(maps.Keys(admitted)) {
		sources = append(sources, meshSource{principal: principal, declared: admitted[principal]})
	}
	return sources
}

// callsThrough reports whether the prepared runnable calls of this
// composition land on one endpoint: the declared call endpoint when the
// environment names one, each owner's own otherwise.
func (index *meshComposition) callsThrough(unique, endpoint string) bool {
	if index.callEndpoint != "" {
		return index.callEndpoint == unique+"/"+endpoint
	}
	module, service, _ := strings.Cut(unique, "/")
	return index.reach.reaches(module, service, endpoint)
}

// hopModuleDeclarers loads the dependency declarers of the hop's own module,
// the ones a refusal names when a principal of that module which is not the
// hop's own asks for a fronted module's listener.
func hopModuleDeclarers(ctx context.Context, workspace *resources.Workspace, module string) ([]resources.DependencyDeclarer, error) {
	modules, err := workspace.LoadModules(ctx)
	if err != nil {
		return nil, err
	}
	for _, mod := range modules {
		if mod == nil || mod.Name != module {
			continue
		}
		declarers, declarerErr := mod.LoadDependencyDeclarers(ctx)
		if declarerErr != nil {
			return nil, fmt.Errorf("load the dependency declarers of the hop's module %s: %w", module, declarerErr)
		}
		return declarers, nil
	}
	return nil, nil
}

// hopModuleAsks names the declarers of the hop's own module that ask for one
// endpoint, as "<kind> <module>/<name>", sorted and each once. It is used in a
// refusal, so a declarer that names the producer service without listing
// endpoints is named for every endpoint of it: the point is to say where to
// look, and a dependency with no endpoint list asks for all of them.
func (index *meshComposition) hopModuleAsks(module, service, endpoint string) []string {
	if index.hop == nil {
		return nil
	}
	var asks []string
	for _, declarer := range index.hopModuleAskers {
		for _, dependency := range declarer.Dependencies {
			if dependency == nil || dependency.Module != module || dependency.Name != service {
				continue
			}
			named := len(dependency.Endpoints) == 0
			for _, reference := range dependency.Endpoints {
				if reference != nil && reference.Name == endpoint {
					named = true
				}
			}
			if !named {
				continue
			}
			asks = append(asks, fmt.Sprintf("%s %s", declarer.Kind, resources.ServiceUnique(index.hop.Module, declarer.Name)))
		}
	}
	slices.Sort(asks)
	return slices.Compact(asks)
}

// manifestPrincipals resolves a service's `admitted-sources` into the mesh
// principals of the callers it names, by endpoint, and the record of every
// admission it resolved.
//
// AN ADMISSION TAKES TWO SIGNATURES. The service's manifest PROPOSES one — it
// is the only side that knows a listener is not an API call — and the
// environment, which the operator owns and the module's authors do not,
// ACCEPTS it (environments.EnvironmentAdmittedSource). A proposal the
// environment does not co-declare refuses the render, naming the entry to add:
// a waiver of the permission layer written in the repository that layer
// protects is one anyone able to land a commit there can grant themselves,
// which is the property standing a hop exists to hold.
//
// And no admission reaches a listener the HOP's route table names: the
// composition already said that listener is published through the hop, so an
// admission on it is not "this port is not an API call" but a second door onto
// the one that is.
//
// A named caller the composition does not carry at all is refused by name — it
// admits nobody wherever it renders. One the composition carries but this
// environment does not run (managed here, or withdrawn by exclude-services)
// omits the edge and SAYS so in the record: an environment that excludes a
// service must not break the render of an unrelated module that admits it,
// which is how derived sources in the same state are already treated.
func (index *meshComposition) manifestPrincipals(env *environments.Environment, module string, service *resources.Service) (map[string][]string, []MeshAdmission, error) {
	declared, err := declaredAdmittedSources(module, service)
	if err != nil {
		return nil, nil, err
	}
	unique := resources.ServiceUnique(module, service.Name)
	principals := make(map[string][]string, len(declared))
	var admissions []MeshAdmission
	for _, endpoint := range slices.Sorted(maps.Keys(declared)) {
		if index.hop.reaches(module, service.Name, endpoint) {
			return nil, nil, fmt.Errorf("%s of %s admits callers on endpoint %s, which the hop %s routes (%s/%s is in its generated route table): that listener is published through the hop by the composition's own statement, so an admission on it is a second door onto the call the hop checks. Admit callers only on a listener the hop does not route, or stop routing it",
				admittedSourcesKey, unique, endpoint, index.hop.unique(), unique, endpoint)
		}
		for _, caller := range declared[endpoint] {
			callerModule, callerService, _ := strings.Cut(caller, "/")
			carries := func(refs []*resources.ServiceWithModule) bool {
				return slices.ContainsFunc(refs, func(ref *resources.ServiceWithModule) bool {
					return ref.Module == callerModule && ref.Name == callerService
				})
			}
			if !carries(index.composed) {
				return nil, nil, fmt.Errorf("%s of %s admits %s on endpoint %s, which the composition does not carry, so the admission admits nobody",
					admittedSourcesKey, unique, caller, endpoint)
			}
			// The manifest's proposal is well formed; now the operator's
			// half. Judged in this order so nobody is told to co-declare a
			// caller that does not exist.
			if !env.AdmitsSource(unique, endpoint, caller) {
				return nil, nil, fmt.Errorf("%s of %s admits %s on endpoint %s, which environment %s does not co-declare: an admission is a waiver of the hop, and a waiver written in the module it protects is one its own code grants itself, so the environment accepts it too. Add it to environment %s:\n  admitted-sources:\n    - service: %s\n      endpoint: %s\n      from: [%s]",
					admittedSourcesKey, unique, caller, endpoint, env.Name, env.Name, unique, endpoint, caller)
			}
			if !carries(index.workloads) {
				admissions = append(admissions, MeshAdmission{
					Module: module, Service: service.Name, Endpoint: endpoint, Caller: caller,
					Omitted: fmt.Sprintf("environment %s runs no workload for it (managed here, or withdrawn by exclude-services), so it has no principal to admit", env.Name),
				})
				continue
			}
			namespace := env.ModuleNamespace(index.workspace, callerModule)
			if namespace == "" {
				return nil, nil, fmt.Errorf("%s of %s admits %s on endpoint %s, whose module has no resolved namespace, so its mesh principal cannot be named",
					admittedSourcesKey, unique, caller, endpoint)
			}
			principal := meshPrincipal(env.Host, namespace, callerService)
			principals[endpoint] = append(principals[endpoint], principal)
			admissions = append(admissions, MeshAdmission{
				Module: module, Service: service.Name, Endpoint: endpoint,
				Caller: caller, Principal: principal,
			})
		}
		slices.Sort(principals[endpoint])
	}
	if err = index.holdDeadCoDeclarations(env, unique, declared); err != nil {
		return nil, nil, err
	}
	return principals, admissions, nil
}

// holdDeadCoDeclarations refuses an acceptance with no proposal: a caller the
// environment co-declares on an endpoint of THIS service that the service's own
// manifest does not admit.
//
// The other direction — a proposal with no acceptance — is refused above, per
// caller, where the entry is resolved. Neither is a warning: a proposal with no
// acceptance would be a waiver module code granted itself, and an acceptance
// with no proposal is an operator believing they admitted a caller the render
// admits nobody for.
//
// It is checked on the render of the module that SERVES the endpoint, since an
// environment entry is not another module's to answer for; every module of a
// composition renders, so the whole set is held.
func (index *meshComposition) holdDeadCoDeclarations(env *environments.Environment, unique string, declared map[string][]string) error {
	for position := range env.AdmittedSources {
		entry := &env.AdmittedSources[position]
		if entry.Service != unique {
			continue
		}
		for _, caller := range entry.From {
			if slices.Contains(declared[entry.Endpoint], caller) {
				continue
			}
			return fmt.Errorf("environment %s co-declares %s admitted to %s on endpoint %s, but %s of %s does not admit it, so the entry admits nobody: an admission is the SERVICE's to propose — the listener is the only side that knows the port is not an API call — and this environment's to accept. Declare it on the endpoint it serves, or drop the entry",
				env.Name, caller, unique, entry.Endpoint, admittedSourcesKey, resources.ServiceConfigurationName)
		}
	}
	return nil
}

// hopAdmission is what one endpoint of a fronted module's service admits, and
// what of the composition's declared reach routes around the hop.
//
// routed is whether the hop's route table names this endpoint, which is the
// only statement of the hop's reach (hop.go): the hop declares no service
// dependency, so nothing derived from the dependency graph carries it. The
// hop's principal is admitted on a routed listener and nowhere else — on a
// listener the table does not name, the authority holds no policy table for
// the call, so admitting the hop there would deliver a door onto an unchecked
// decision.
//
// A principal of the hop's OWN MODULE is not the hop's. The derived allow-list
// is at module granularity, so a SIBLING service of the hop that declares a
// dependency on a fronted module's listener arrives here as a principal of the
// hop's module — and it is refused by name like any other route around,
// naming the declarer that asked (hopModuleAsks). It was once collapsed into
// the hop's principal instead, on the reasoning that no derivation can tell a
// sibling's ask from the hop's; the reasoning does not hold — the hop declares
// no dependency at all, so every derived principal of its module IS a
// sibling's ask — and the collapse did two wrong things at once: it admitted
// the hop to listeners its route table does not name, and it deleted the
// sibling's declared reach, leaving a module that fails at runtime with
// nothing said at render time.
func (index *meshComposition) hopAdmission(module, service, endpoint string, derived []meshSource, declared []string, self string, selfConsuming, routed bool) (admitted []string, around []string) {
	admit := map[string]bool{}
	// The hop reaches exactly the listeners its own route table names.
	if routed {
		admit[index.hopPrincipal] = true
	}
	// Which declarers of the hop's own module ask for this listener. The
	// derived allow-list is at module granularity, so every principal of that
	// module arrives for ONE of these asks — the hop's own principal
	// included, since the hop declares no service dependency of its own.
	asks := index.hopModuleAsks(module, service, endpoint)
	hopModule := map[string]bool{}
	for _, principal := range index.byModule[index.hop.Module] {
		hopModule[principal] = true
	}
	for _, source := range derived {
		if slices.Contains(declared, source.principal) || (selfConsuming && source.principal == self) {
			admit[source.principal] = true
			continue
		}
		if source.principal == index.hopPrincipal && routed {
			admit[source.principal] = true
			continue
		}
		why := source.declared
		switch {
		case source.principal == index.hopPrincipal && len(asks) > 0:
			// Not the hop's own ask: the hop declares no dependency, so the
			// module-granular list carried a sibling's ask onto the hop's
			// principal too. Say whose, and that the table does not name the
			// listener, or this reads as the hop being refused its own route.
			why = fmt.Sprintf("%s — the ask is %s's, which a module-granular allow-list cannot separate from the hop's, and the hop's route table does not name this listener",
				source.declared, strings.Join(asks, ", "))
		case source.principal == index.hopPrincipal:
			why = fmt.Sprintf("%s, and the hop's route table does not name this listener", source.declared)
		case hopModule[source.principal] && len(asks) > 0:
			why = fmt.Sprintf("%s — %s, a sibling of the hop in the hop's own module, declares it", source.declared, strings.Join(asks, ", "))
		}
		around = append(around, fmt.Sprintf("%s: %s", source.principal, why))
	}
	for _, principal := range declared {
		admit[principal] = true
	}
	if selfConsuming {
		admit[self] = true
	}
	return slices.Sorted(maps.Keys(admit)), around
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
	// Behind a hop, each endpoint of this module's services admits the hop and
	// the callers the service's own manifest names, and nothing else.
	behindTheHop := index.hop.fronts(module.Name)
	// The hop rule is the DEPLOYED rule, and the one environment it is lifted
	// for is a developer's own cluster — where calling a module directly is
	// the point. That is a DECLARED local k3d cluster (cluster.kind: k3d and a
	// k3d-… context), never an environment whose name happens to start with
	// "local": `local-eu`, `local-shared` and `localization` are deployed
	// cells as far as anything but a name can tell, and omitting a field must
	// narrow what an environment may do, never widen it. It is the same gate
	// that decides whether a host may admit a developer identity or mount a
	// local trust anchor (environments.DeclaresLocalK3d).
	developerCluster := env.DeclaresLocalK3d()
	record := &MeshEdges{SchemaVersion: MeshEdgesSchemaVersion, TrustDomain: env.Host.TrustDomain, Gateway: env.Host.Gateway}
	for _, service := range services {
		if service == nil {
			continue
		}
		if _, managed := env.ManagedService(module.Name, service.Name); managed {
			continue
		}
		unique := resources.ServiceUnique(module.Name, service.Name)
		// The manifest's own admissions are read and checked for every
		// service of every environment, hop or none: a declaration that names
		// an endpoint or a caller the composition does not have is a mistake
		// wherever it renders.
		admittedByManifest, admissions, err := index.manifestPrincipals(env, module.Name, service)
		if err != nil {
			return nil, err
		}
		record.Admissions = append(record.Admissions, admissions...)
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
			derived := index.sources(module.Name, service.Name, endpoint.Name)
			var principals []string
			if behindTheHop && !developerCluster {
				var around []string
				principals, around = index.hopAdmission(module.Name, service.Name, endpoint.Name, derived,
					admittedByManifest[endpoint.Name], principal,
					selfConsuming[unique], index.hop.reaches(module.Name, service.Name, endpoint.Name))
				if len(around) > 0 {
					return nil, fmt.Errorf("%s/%s is published through the hop %s, so only the hop reaches it, but the composition routes %d caller(s) straight to its listener on environment %s — %s. Route the call through the hop (an environment's runnable-call-endpoint sends every prepared call there; a consumer declares no dependency on a module service), or, where the port is not an API call, admit the caller on the endpoint it serves (%s in %s, co-declared by the environment)",
						unique, endpoint.Name, index.hop.unique(), len(around), env.Name, strings.Join(around, "; "), admittedSourcesKey, resources.ServiceConfigurationName)
				}
			} else {
				for _, source := range derived {
					principals = append(principals, source.principal)
				}
				// An admission is applied wherever it was declared and
				// accepted, hop or none. It was once read for every service
				// and APPLIED only behind a hop on a non-local environment,
				// which made a declaration the render validated and then
				// ignored: a module outside hop/MODULES, or one taken out of
				// it, silently lost its admitted callers and was denied at
				// the mesh with everything in the composition saying it was
				// allowed. A security declaration is never validate-and-drop.
				principals = append(principals, admittedByManifest[endpoint.Name]...)
				// A non-serving workload of this service runs as this service
				// and reaches this endpoint; the composition never declares
				// it, so the render is where it enters.
				if selfConsuming[unique] && !slices.Contains(principals, principal) {
					principals = append(principals, principal)
				}
				slices.Sort(principals)
				principals = slices.Compact(principals)
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
			// AND NOT ONTO A LISTENER THE HOP ROUTES. An ingress route is the
			// cell's own edge, not a principal, which is why it is otherwise
			// untouched by the hop rule — but the edge is reachable from
			// inside the cell too, so a route onto a hop-routed listener is a
			// hairpin around the hop that needs only a Host header, and the
			// module declaring that listener `public` does not make the call
			// on it a browser's. The composition said this listener is
			// published through the hop; the edge entrance is some other
			// endpoint.
			if ingress && index.hop.reaches(module.Name, service.Name, endpoint.Name) {
				return nil, fmt.Errorf("the environment routes %s/%s at its edge, but the hop %s routes that listener (it is in the hop's generated route table), so a client inside the cell reaches it through the ingress gateway with a Host header and never through the hop. Route the edge at the hop, or at an endpoint the hop does not route",
					unique, endpoint.Name, index.hop.unique())
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
	sort.Slice(record.Admissions, func(i, j int) bool {
		left, right := &record.Admissions[i], &record.Admissions[j]
		if left.Service != right.Service {
			return left.Service < right.Service
		}
		if left.Endpoint != right.Endpoint {
			return left.Endpoint < right.Endpoint
		}
		return left.Caller < right.Caller
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
