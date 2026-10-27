package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/authority/libraries/hop/go/routing"
	"github.com/codefly-dev/cli/pkg/methodpolicy"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/configurations"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

const (
	hopRoutesGroup = "hop-routes"
	// hopRoutesKey is the one value of that group: the route table, or empty
	// where the composition names no module for the hop.
	hopRoutesKey = "ROUTES"
)

// WorkspaceConfigurationReader adds a generated, non-root group to the ordinary Core
// reader. Both planning and delivery see its unresolved endpoint references,
// so Core owns graph edges, visibility, readiness and address interpolation.
type WorkspaceConfigurationReader struct {
	*configurations.ConfigurationInformationLocalReader
	workspace *resources.Workspace
	combined  []*basev0.Configuration
	snapshot  string
	frozen    bool
}

// NewWorkspaceConfigurationReader shares generated and authored configuration inputs
// between planning, run delivery and deployment consumer policies.
func NewWorkspaceConfigurationReader(ctx context.Context, workspace *resources.Workspace) (*WorkspaceConfigurationReader, error) {
	reader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	if err != nil {
		return nil, err
	}
	return &WorkspaceConfigurationReader{ConfigurationInformationLocalReader: reader, workspace: workspace}, nil
}

func (r *WorkspaceConfigurationReader) Configurations() []*basev0.Configuration { return r.combined }

func (r *WorkspaceConfigurationReader) Load(ctx context.Context, env *resources.Environment) error {
	if err := r.ConfigurationInformationLocalReader.Load(ctx, env); err != nil {
		return err
	}
	configs := r.ConfigurationInformationLocalReader.Configurations()
	group, err := generatedHopRoutes(ctx, r.workspace, configs)
	if err != nil {
		return err
	}
	snapshot := hopRoutingSnapshot(group)
	if r.frozen && r.snapshot != snapshot {
		return routingRestartRequired("the published API catalogs differ from the ones this run started with")
	}
	r.snapshot, r.frozen = snapshot, true
	r.combined = slices.Clone(configs)
	if group != nil {
		r.combined = append(r.combined, group)
	}
	// The one fixture-composition document of this composition, merged from
	// the composed modules' fragments: the group is multi-provider by design,
	// and this is its provider where the workspace has no answer of its own
	// (fixture_composition.go).
	fixture, err := mergedFixtureComposition(ctx, r.workspace, env, configs)
	if err != nil {
		return err
	}
	if fixture != nil {
		r.combined = append(r.combined, fixture)
	}
	return nil
}

func routingRestartRequired(reason string) error {
	return fmt.Errorf("published hop routes changed: stop and restart this Codefly run to adopt them; incremental refresh cannot replace its routing snapshot (%s)", reason)
}

func hopRoutingSnapshot(config *basev0.Configuration) string {
	if config == nil {
		return ""
	}
	for _, info := range config.GetInfos() {
		if info.GetName() == hopRoutesGroup {
			for _, value := range info.GetConfigurationValues() {
				if value.GetKey() == hopRoutesKey {
					return value.GetValue()
				}
			}
		}
	}
	return ""
}

func hopRoutingSnapshotOf(infos []*basev0.ConfigurationInformation) string {
	return hopRoutingSnapshot(&basev0.Configuration{Infos: infos})
}

func generatedHopRoutes(ctx context.Context, workspace *resources.Workspace, configs []*basev0.Configuration) (*basev0.Configuration, error) {
	var listed string
	for _, info := range workspaceConfigurationInfos(configs) {
		if info.GetName() == hopRoutesGroup {
			return nil, fmt.Errorf("%s is generated from published API catalogs; remove its authored configuration", hopRoutesGroup)
		}
		if info.GetName() == "hop" {
			for _, value := range info.GetConfigurationValues() {
				if value.GetKey() == "MODULES" {
					listed = value.GetValue()
				}
			}
		}
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(services, func(s *resources.Service) bool {
		return slices.Contains(s.WorkspaceConfigurationDependencies, hopRoutesGroup)
	}) {
		return nil, nil
	}
	// A declared group that resolves to nothing is an error in Core's reader
	// (GetWorkspaceDependenciesConfigurations), raised from the DECLARATION at
	// runtime-init, before any of the service's code runs — so a host that
	// stands a hop on some compositions and not others cannot degrade, and
	// cannot stop declaring the group either, since generation requires a
	// declaring service. It is handed the EMPTY view instead: nothing decodes
	// an empty ROUTES (a host stands its hop only where the hop group names
	// modules), the routing snapshot of an empty value is the same as of no
	// group at all, so no run is told its routes changed, and a composition
	// that does name modules still receives the routes generated below.
	if listed == "" {
		return emptyHopRoutes(), nil
	}
	routes := routing.Routing{Schema: routing.RoutingSchema}
	modules := strings.Split(listed, ",")
	for i := range modules {
		modules[i] = strings.TrimSpace(modules[i])
	}
	slices.Sort(modules)
	for i, name := range modules {
		if name == "" || (i > 0 && name == modules[i-1]) {
			return nil, fmt.Errorf("hop/MODULES contains an empty or duplicate principal")
		}
		module, loadErr := workspace.LoadModuleFromName(ctx, name)
		if loadErr != nil {
			return nil, fmt.Errorf("hop routes for %s: %w", name, loadErr)
		}
		endpoints, routeErr := publishedHopEndpoints(ctx, module)
		if routeErr != nil {
			return nil, fmt.Errorf("hop routes for %s: %w", name, routeErr)
		}
		routes.Endpoints = append(routes.Endpoints, endpoints...)
	}
	if err = routes.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(routes)
	if err != nil {
		return nil, err
	}
	return &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{Name: hopRoutesGroup, ConfigurationValues: []*basev0.ConfigurationValue{{Key: hopRoutesKey, Value: string(raw)}}}}}, nil
}

// emptyHopRoutes is the hop-routes group holding no route: what a service
// declaring the group receives where the composition names no module for it.
func emptyHopRoutes() *basev0.Configuration {
	return &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
		Name: hopRoutesGroup, ConfigurationValues: []*basev0.ConfigurationValue{{Key: hopRoutesKey, Value: ""}},
	}}}
}

func publishedHopEndpoints(ctx context.Context, module *resources.Module) ([]routing.RouteEndpoint, error) {
	contract, err := modulecontract.Load(module.Dir())
	if err != nil {
		return nil, err
	}
	if contract.Principal != module.Name {
		return nil, fmt.Errorf("contract principal %q differs from composed module %q", contract.Principal, module.Name)
	}
	manifest, err := composition.LoadPackageManifest(module.Dir())
	if err != nil {
		return nil, err
	}
	catalog, err := composition.LoadAPIContractCatalog(module.Dir())
	if err != nil {
		return nil, err
	}
	if catalog.Package != manifest.ID || catalog.Version != manifest.Version {
		return nil, fmt.Errorf("API catalog package/version differs from its package manifest")
	}
	if err := composition.ValidatePackageAPIContracts(module.Dir(), manifest, catalog); err != nil {
		return nil, err
	}
	var out []routing.RouteEndpoint
	for i := range catalog.Endpoints {
		endpoint := &catalog.Endpoints[i]
		service, loadErr := module.LoadServiceFromName(ctx, endpoint.Service)
		if loadErr != nil {
			return nil, loadErr
		}
		idx := slices.IndexFunc(service.Endpoints, func(e *resources.Endpoint) bool { return e.Name == endpoint.Endpoint })
		if idx < 0 || service.Endpoints[idx].API != endpoint.API {
			return nil, fmt.Errorf("catalog listener %s/%s does not match the selected service endpoint", endpoint.Service, endpoint.Endpoint)
		}
		entry := routing.RouteEndpoint{Audience: contract.Principal, Service: endpoint.Service, Endpoint: endpoint.Endpoint, Transport: endpoint.API,
			Address: "${endpoint:" + module.Name + "/" + endpoint.Service + "/" + endpoint.Endpoint + "}", Package: catalog.Package, Version: catalog.Version, Digest: endpoint.Digest}
		published, findErr := publishedContract(manifest, endpoint)
		if findErr != nil {
			return nil, findErr
		}
		// The methods are the policy table the authority receives for this
		// listener (methodpolicy, the reader of every delivered table), read
		// from the digest-checked contract bytes, never the catalog's own route
		// list: a route the hop maps onto is a route the authority holds a
		// policy for, spelled the same way, basePath included.
		policies, _, policyErr := methodpolicy.Contract(module.Dir(), catalog, endpoint.Service, published)
		if policyErr != nil {
			return nil, fmt.Errorf("catalog listener %s/%s: %w", endpoint.Service, endpoint.Endpoint, policyErr)
		}
		switch {
		case endpoint.Kind == composition.APIContractKindProtobuf && (endpoint.API == "grpc" || endpoint.API == "connect"):
			for _, policy := range policies {
				// A procedure; its google.api.http routes belong to an HTTP face.
				if strings.HasPrefix(policy.GetMethod(), "/") {
					entry.Methods = append(entry.Methods, policy.GetMethod())
				}
			}
		case endpoint.Kind == composition.APIContractKindOpenAPI && (endpoint.API == "http" || endpoint.API == "rest"):
			entry.Transport = "http"
			for _, policy := range policies {
				entry.Methods = append(entry.Methods, policy.GetMethod())
			}
		default:
			return nil, fmt.Errorf("catalog listener %s/%s has unsupported %s/%s publication", endpoint.Service, endpoint.Endpoint, endpoint.API, endpoint.Kind)
		}
		slices.Sort(entry.Methods)
		out = append(out, entry)
	}
	if err := routeForeignMethods(out, contract.ForeignMethods); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b routing.RouteEndpoint) int {
		return strings.Compare(a.Service+"/"+a.Endpoint, b.Service+"/"+b.Endpoint)
	})
	return out, nil
}

// routeForeignMethods adds the module contract's foreign methods — served
// from a protocol the module does not own, so no published contract of its
// declares them — to the listeners that serve them, so the routes hold the
// same method set as the policy table the authority receives
// (gitops.ServiceMethodPolicies delivers them). A foreign method names no
// service: it is served by the module's one service publishing protobuf over
// gRPC or Connect, on each of those listeners, which the hop treats as one
// destination. A module where none or several services do is refused by name
// rather than guessed, as is a foreign method a published contract already
// declares.
func routeForeignMethods(endpoints []routing.RouteEndpoint, foreign []modulecontract.ForeignMethod) error {
	if len(foreign) == 0 {
		return nil
	}
	rpc := func(e *routing.RouteEndpoint) bool { return e.Transport == "grpc" || e.Transport == "connect" }
	var services []string
	for i := range endpoints {
		if rpc(&endpoints[i]) && !slices.Contains(services, endpoints[i].Service) {
			services = append(services, endpoints[i].Service)
		}
	}
	if len(services) != 1 {
		methods := make([]string, 0, len(foreign))
		for _, method := range foreign {
			methods = append(methods, method.Method)
		}
		slices.Sort(services)
		return fmt.Errorf("its module contract declares foreign methods %s, which name no service, and %d services publish gRPC or Connect contracts %v: the hop cannot tell which one serves them",
			strings.Join(methods, ", "), len(services), services)
	}
	for i := range endpoints {
		endpoint := &endpoints[i]
		if !rpc(endpoint) {
			continue
		}
		for _, method := range foreign {
			if slices.Contains(endpoint.Methods, method.Method) {
				return fmt.Errorf("foreign method %s is also declared by the contract %s/%s publishes", method.Method, endpoint.Service, endpoint.Endpoint)
			}
			endpoint.Methods = append(endpoint.Methods, method.Method)
		}
		slices.Sort(endpoint.Methods)
	}
	return nil
}

// publishedContract is the package manifest's contract for a catalog
// listener, under the same digest: the bytes the authority's table is read from.
func publishedContract(manifest *composition.PackageManifest, endpoint *composition.APIContractEndpoint) (*composition.ProvidedAPIContract, error) {
	for i := range manifest.Services {
		service := &manifest.Services[i]
		if service.Name != endpoint.Service {
			continue
		}
		for j := range service.APIContracts {
			contract := &service.APIContracts[j]
			if contract.Endpoint == endpoint.Endpoint && contract.Kind == endpoint.Kind {
				if contract.Digest != endpoint.Digest {
					return nil, fmt.Errorf("catalog listener %s/%s records %s, the package manifest publishes %s", endpoint.Service, endpoint.Endpoint, endpoint.Digest, contract.Digest)
				}
				return contract, nil
			}
		}
	}
	return nil, fmt.Errorf("catalog listener %s/%s has no %s contract in the package manifest", endpoint.Service, endpoint.Endpoint, endpoint.Kind)
}

// CheckRoutingSnapshot must run before withdrawal, declaration admission or
// extension. A rejected candidate leaves the accepted running composition alone.
func (flow *Flow) CheckRoutingSnapshot(ctx context.Context, workspace *resources.Workspace) error {
	if flow.world == nil || flow.world.Env == nil {
		return nil
	}
	provided, _, err := WorkspaceConfigurationsForChecking(ctx, workspace, flow.world.Env)
	if err != nil {
		return err
	}
	var previous, current string
	if flow.providedWorkspaceConfigurations != nil {
		previous = hopRoutingSnapshotOf(flow.providedWorkspaceConfigurations.Infos)
	}
	if provided != nil {
		current = hopRoutingSnapshotOf(provided.Infos)
	}
	if previous != current {
		return routingRestartRequired("the published API catalogs differ from the ones this run started with")
	}
	return nil
}

// freezeHopAddresses fences accepted endpoint replacement while an immutable
// hop snapshot can still use it. Ordinary reload reuses the accepted mappings.
//
// A routed service that has initialized must still be at the address it
// recorded. One that has loaded but not initialized yet — a fresh run starts
// the hop before every module ordered after it, and a module that calls
// through the hop is — is routed at the address its own Init will be
// proposed, the one the hop's resolution derived. Freezing that address makes
// the routed service's Init the check (RecordNetworkMappings): the same
// address is accepted, any other is refused naming the route. A service that
// never loaded has no mapping at all and is refused here by name.
//
// Until that Init, the hop routes to a derived loopback port nothing holds
// yet, so a local process that binds it first receives what the hop forwards
// until then. That Init ends it: before calling the agent, the routed
// service's first Init dials the port it is proposed on 127.0.0.1 and ::1 and
// refuses, naming the holder, if anything answers
// (Runner.checkInitialPortAvailability), so the run never comes up with the
// hop routing to a squatter. --temporary-ports never derives an address at
// all. A process that binds a loopback address on the same port after that
// probe, or a routed service that crashes or is withdrawn mid-run, is not
// covered.
func (s *StateManager) freezeHopAddresses(raw string, resolved []*basev0.NetworkMapping) error {
	routes, err := routing.DecodeRouting(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := map[string]frozenRoute{}
	for i := range routes.Endpoints {
		route := &routes.Endpoints[i]
		unique := route.Audience + "/" + route.Service
		var selected *basev0.NetworkMapping
		for _, mapping := range resolved {
			e := mapping.GetEndpoint()
			if e.GetModule()+"/"+e.GetService() == unique && e.GetName() == route.Endpoint {
				selected = mapping
				break
			}
		}
		if selected == nil {
			return fmt.Errorf("hop route %s/%s has no resolved mapping", unique, route.Endpoint)
		}
		key, hash := resources.EndpointDestination(selected.GetEndpoint()), resources.NetworkMappingHash(selected)
		if recorded, initialized := s.networkMappings[unique]; initialized && !carriesMapping(recorded, key, hash) {
			// One record means the hop resolved the address that first Init
			// was proposed: a restart proposes it again.
			if s.networkMappingRecords[unique] == 1 {
				return proposedAddressRefused(key, mappingAddresses(selected), unique, addressesOf(recorded, key))
			}
			return routingRestartRequired(fmt.Sprintf("hop route %s was resolved at %s, but %s initialized at %s while the routes were being resolved",
				key, mappingAddresses(selected), unique, addressesOf(recorded, key)))
		}
		if previous, ok := s.routingMappings[key]; ok && previous.hash != hash {
			return routingRestartRequired(fmt.Sprintf("hop route %s moved from %s, the address this run's hop snapshot holds, to %s",
				key, mappingAddresses(previous.mapping), mappingAddresses(selected)))
		}
		pending[key] = frozenRoute{owner: unique, hash: hash, mapping: selected}
	}
	if s.routingMappings == nil {
		s.routingMappings = map[string]frozenRoute{}
	}
	for key, frozen := range pending {
		s.routingMappings[key] = frozen
	}
	return nil
}

// frozenRoute is one address a hop snapshot holds: the service that serves it,
// the mapping the snapshot was built with, and that mapping's hash.
type frozenRoute struct {
	owner   string
	hash    string
	mapping *basev0.NetworkMapping
}

// checkFrozenRoutes refuses mappings for unique that drop or move an address a
// hop snapshot holds. The caller holds s.mu and has not recorded them yet.
//
// A service recording for the first time was frozen at the address its Init
// was proposed, and that address is derived from the workspace, the service
// and the naming scope: a restart derives it again and is refused the same
// way, so the refusal says what does help instead of asking for one.
func (s *StateManager) checkFrozenRoutes(unique string, mappings []*basev0.NetworkMapping) error {
	_, initialized := s.networkMappings[unique]
	for key, frozen := range s.routingMappings {
		if frozen.owner != unique || carriesMapping(mappings, key, frozen.hash) {
			continue
		}
		routed, offered := mappingAddresses(frozen.mapping), addressesOf(mappings, key)
		if initialized {
			return routingRestartRequired(fmt.Sprintf("hop route %s is routed at %s, but %s now offers %s", key, routed, unique, offered))
		}
		return proposedAddressRefused(key, routed, unique, offered)
	}
	return nil
}

// proposedAddressRefused is the refusal of a first Init that accepted another
// address than the one it was proposed, which the hop routes. A restart
// proposes the same derived address again, so it is not offered as a remedy.
func proposedAddressRefused(key, routed, unique, offered string) error {
	return fmt.Errorf("hop route %s is routed at %s, the address %s was proposed, but its Init accepted %s: "+
		"the proposed address is derived from the workspace, the service and the naming scope, so restarting this run derives it again and is refused the same way; "+
		"have %s's agent serve the proposed address, or, if %s does not call through the hop, order it before the hop", key, routed, unique, offered, unique, unique)
}

// carriesMapping reports whether mappings hold the endpoint key, and hold it
// only at the mapping hashed as hash.
func carriesMapping(mappings []*basev0.NetworkMapping, key, hash string) bool {
	found := false
	for _, mapping := range mappings {
		if resources.EndpointDestination(mapping.GetEndpoint()) != key {
			continue
		}
		if resources.NetworkMappingHash(mapping) != hash {
			return false
		}
		found = true
	}
	return found
}

// addressesOf spells where mappings reach the endpoint key.
func addressesOf(mappings []*basev0.NetworkMapping, key string) string {
	var matching []*basev0.NetworkMapping
	for _, mapping := range mappings {
		if resources.EndpointDestination(mapping.GetEndpoint()) == key {
			matching = append(matching, mapping)
		}
	}
	if len(matching) == 0 {
		return "no address for it"
	}
	return mappingAddresses(matching...)
}

// mappingAddresses spells each instance of mappings as its access and address.
func mappingAddresses(mappings ...*basev0.NetworkMapping) string {
	var parts []string
	for _, mapping := range mappings {
		for _, instance := range mapping.GetInstances() {
			parts = append(parts, instance.GetAccess().GetKind()+" "+instance.GetAddress())
		}
	}
	if len(parts) == 0 {
		return "no address"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (world *World) freezeDeliveredHopAddresses(configs []*basev0.Configuration, mappings []*basev0.NetworkMapping) error {
	if world.Mode != RunMode || world.SharedState == nil {
		return nil
	}
	for _, config := range configs {
		if raw := hopRoutingSnapshot(config); raw != "" {
			if err := world.SharedState.freezeHopAddresses(raw, mappings); err != nil {
				return err
			}
		}
	}
	return nil
}
