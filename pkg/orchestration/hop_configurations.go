package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/authority/libraries/hop/go/routing"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/configurations"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

const hopRoutesGroup = "hop-routes"

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
		return routingRestartRequired()
	}
	r.snapshot, r.frozen = snapshot, true
	r.combined = slices.Clone(configs)
	if group != nil {
		r.combined = append(r.combined, group)
	}
	return nil
}

func routingRestartRequired() error {
	return fmt.Errorf("published hop routes changed: stop and restart this Codefly run to adopt them; incremental refresh cannot replace its routing snapshot")
}

func hopRoutingSnapshot(config *basev0.Configuration) string {
	if config == nil {
		return ""
	}
	for _, info := range config.GetInfos() {
		if info.GetName() == hopRoutesGroup {
			for _, value := range info.GetConfigurationValues() {
				if value.GetKey() == "ROUTES" {
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
	if listed == "" {
		return nil, nil
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
	return &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{Name: hopRoutesGroup, ConfigurationValues: []*basev0.ConfigurationValue{{Key: "ROUTES", Value: string(raw)}}}}}, nil
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
		switch {
		case endpoint.Kind == composition.APIContractKindProtobuf && (endpoint.API == "grpc" || endpoint.API == "connect"):
			for _, svc := range endpoint.Services {
				entry.Methods = append(entry.Methods, svc.Procedures...)
			}
		case endpoint.Kind == composition.APIContractKindOpenAPI && (endpoint.API == "http" || endpoint.API == "rest"):
			entry.Transport = "http"
			for _, route := range endpoint.Routes {
				entry.Methods = append(entry.Methods, route.Method+" "+route.Path)
			}
		default:
			return nil, fmt.Errorf("catalog listener %s/%s has unsupported %s/%s publication", endpoint.Service, endpoint.Endpoint, endpoint.API, endpoint.Kind)
		}
		slices.Sort(entry.Methods)
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b routing.RouteEndpoint) int {
		return strings.Compare(a.Service+"/"+a.Endpoint, b.Service+"/"+b.Endpoint)
	})
	return out, nil
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
		return routingRestartRequired()
	}
	return nil
}

// freezeHopAddresses fences accepted endpoint replacement while an immutable
// hop snapshot can still use it. Ordinary reload reuses the accepted mappings.
func (s *StateManager) freezeHopAddresses(raw string, resolved []*basev0.NetworkMapping) error {
	routes, err := routing.DecodeRouting(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := map[string]string{}
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
		current := false
		for _, mapping := range s.networkMappings[unique] {
			if resources.EndpointDestination(mapping.GetEndpoint()) == key && resources.NetworkMappingHash(mapping) == hash {
				current = true
				break
			}
		}
		if !current {
			return routingRestartRequired()
		}
		if previous, ok := s.routingMappings[key]; ok && previous != hash {
			return routingRestartRequired()
		}
		pending[key] = hash
	}
	if s.routingMappings == nil {
		s.routingMappings = map[string]string{}
	}
	for key, hash := range pending {
		s.routingMappings[key] = hash
	}
	return nil
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
