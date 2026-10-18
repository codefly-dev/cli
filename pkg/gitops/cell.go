package gitops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/run/fixture"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"gopkg.in/yaml.v3"
)

// --- The cell file ---
//
// A cell is one deployment host and everything delivered to it. The cell's
// infrastructure derives its policy — which workload may reach which endpoint,
// under which identity, on which port, and what each may reach outside — from
// an inventory of what is delivered, rather than from a hand-written set that
// drifts the moment a module is added. The cell file is that inventory, emitted
// by the render from the composition and the trees it has rendered, for one
// environment, in the shape the consumer reads (pkg/gitops/cellfile).
//
// It is regenerated whole after every module render, so absence is correct: a
// workload not in the file is not delivered. It cannot live in a module's owned
// tree, where the promotable ruleset admits only Kubernetes manifests and
// Kustomizations; it lives beside the module trees, under
// <gitops path>/cells/<environment>/cell.yaml, and is committed with them.

const cellsDir = "cells"

// CellResult reports what a cell render wrote and what it left out.
type CellResult struct {
	// Path is the cell file written.
	Path string
	// Modules are the modules whose rendered trees the file covers.
	Modules []string
	// Skipped names module trees rendered for another environment, which
	// describe another cell.
	Skipped []string
	// Source reports whether the file names its repository, branch and base
	// revision. It does not when the environment declares no gitops
	// repository or the workspace is not a git checkout.
	Source bool
	// ConfiguredEdges says, one line each, the consumer edges a service's
	// configuration declares (an ${endpoint:…} it reads) that no service
	// dependency does.
	ConfiguredEdges []string
}

// cellRoot is the gitops root the cell file is written under: the
// environment's declared path, the workspace default, or deployments.
func cellRoot(workspace *resources.Workspace, env *environments.Environment) (string, error) {
	root := "deployments"
	if env.Gitops != nil && env.Gitops.Path != "" {
		root = env.Gitops.Path
	} else if defaults, err := environments.WorkspaceGitops(workspace); err != nil {
		return "", err
	} else if defaults != nil && defaults.Path != "" {
		root = defaults.Path
	}
	return filepath.Join(workspace.Dir(), filepath.FromSlash(root)), nil
}

// CellPath is the cell file of one environment.
func CellPath(workspace *resources.Workspace, env *environments.Environment) (string, error) {
	root, err := cellRoot(workspace, env)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, cellsDir, env.Name, cellfile.FileName), nil
}

// RenderCell derives the environment's cell file from every module tree
// rendered for that environment under the workspace's module renders, and the
// composition that produced them, and writes it. It runs after a module
// render, so the file always describes the trees as they stand.
func RenderCell(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (CellResult, error) {
	file, result, err := deriveCell(ctx, workspace, env)
	if err != nil {
		return CellResult{}, err
	}
	if err := writeCell(&file, result.Path); err != nil {
		return CellResult{}, err
	}
	return result, nil
}

// deriveCell is RenderCell without the write: the cell file the rendered
// trees and the composition describe, and the path it belongs at.
func deriveCell(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (cellfile.File, CellResult, error) {
	if workspace == nil || env == nil {
		return cellfile.File{}, CellResult{}, errors.New("a cell is rendered for a workspace and an environment")
	}
	if env.Host == nil {
		return cellfile.File{}, CellResult{}, fmt.Errorf("environment %s declares no host, so it has no cell to describe", env.Name)
	}
	modulesDir := filepath.Dir(moduleRenderDestination(workspace, "module"))
	entries, err := os.ReadDir(modulesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return cellfile.File{}, CellResult{}, fmt.Errorf("no module has been rendered under %s", modulesDir)
	}
	if err != nil {
		return cellfile.File{}, CellResult{}, fmt.Errorf("list rendered modules: %w", err)
	}
	consumers, configured, err := endpointConsumers(ctx, workspace, env)
	if err != nil {
		return cellfile.File{}, CellResult{}, err
	}
	file := cellfile.File{
		Schema: cellfile.Schema, Environment: env.Name,
		Coordinate: env.Host.Coordinate, Component: env.Host.Component, Domain: env.Host.Domain, TrustDomain: env.Host.TrustDomain,
		Source: cellSource(ctx, workspace, env),
	}
	result := CellResult{Source: file.Source != nil, ConfiguredEdges: configured}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		tree := filepath.Join(modulesDir, entry.Name())
		inventory, loadErr := LoadInventory(tree)
		if errors.Is(loadErr, fs.ErrNotExist) {
			continue
		}
		if loadErr != nil {
			return cellfile.File{}, CellResult{}, fmt.Errorf("the render of module %s cannot be read, so the cell file cannot describe what it delivers; render %s again for %s: %w", entry.Name(), entry.Name(), env.Name, loadErr)
		}
		if inventory.Environment != env.Name {
			result.Skipped = append(result.Skipped, inventory.Module)
			continue
		}
		namespace, namespaceErr := cellNamespace(ctx, workspace, env, tree, &inventory, consumers)
		if namespaceErr != nil {
			return cellfile.File{}, CellResult{}, namespaceErr
		}
		file.Namespaces = append(file.Namespaces, namespace)
		result.Modules = append(result.Modules, inventory.Module)
	}
	if len(file.Namespaces) == 0 {
		return cellfile.File{}, CellResult{}, fmt.Errorf("no module is rendered for environment %s under %s", env.Name, modulesDir)
	}
	sort.Slice(file.Namespaces, func(i, j int) bool { return file.Namespaces[i].Name < file.Namespaces[j].Name })
	sort.Strings(result.Modules)
	sort.Strings(result.Skipped)
	if result.Path, err = CellPath(workspace, env); err != nil {
		return cellfile.File{}, CellResult{}, err
	}
	return file, result, nil
}

// writeCell encodes the cell file and writes it at path.
func writeCell(file *cellfile.File, path string) error {
	body, err := file.Encode()
	if err != nil {
		return fmt.Errorf("the rendered cell cannot be written: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create the cell directory: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write the cell file: %w", err)
	}
	return nil
}

// cellSource is the repository and publication branch the environment's
// gitops declaration names, and the revision the render was BASED on: the
// workspace checkout's HEAD when it ran. That is the parent of the commit that
// will hold the rendered trees and this file — a render cannot name the commit
// it will be committed in — and equals it only for a re-render that changed
// nothing. A consumer that binds a delivery to an exact commit sets the
// revision itself when it pins the file (infra-base's pin, which requires the
// field and writes the commit it reviewed); a cell tracking the branch reads
// the branch head. Nil when either half is missing, which the result reports
// rather than inventing a revision.
func cellSource(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) *cellfile.Source {
	if env.Gitops == nil || env.Gitops.RepoURL == "" || env.Gitops.Branch == "" {
		return nil
	}
	head, err := gitCommandBytes(ctx, workspace.Dir(), "rev-parse", "HEAD")
	if err != nil {
		return nil
	}
	revision := strings.TrimSpace(string(head))
	if len(revision) != 40 {
		return nil
	}
	return &cellfile.Source{Repo: env.Gitops.RepoURL, Revision: revision, Branch: env.Gitops.Branch}
}

// cellNamespace derives one module's namespace entry from its rendered tree
// and the composition's declarations for it.
func cellNamespace(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, tree string, inventory *Inventory, consumers map[string][]string) (cellfile.Namespace, error) {
	namespace := cellfile.Namespace{Name: inventory.Namespace, Module: inventory.Module}
	if namespace.Name == "" {
		return cellfile.Namespace{}, fmt.Errorf("the render of module %s names no namespace", inventory.Module)
	}
	var release *cellfile.Release
	if inventory.Package != nil {
		if publisher, name, found := strings.Cut(inventory.Package.ID, "/"); found {
			release = &cellfile.Release{Publisher: publisher, Name: name, Version: inventory.Package.Version}
		}
	}
	for index := range inventory.Units {
		unit := &inventory.Units[index]
		// Every pod-producing unit is inventoried: a managed service's
		// bootstrap Job runs under the same closed admission set as a
		// serving workload. What differs is only that a managed unit
		// declares no endpoints of its own.
		if unit.Path == "" {
			continue
		}
		unitDir := filepath.Join(tree, filepath.FromSlash(unit.Path))
		var service *resources.Service
		if !unit.Managed && unit.Kind == UnitKindService {
			loaded, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: unit.Name, Module: inventory.Module})
			if err != nil {
				return cellfile.Namespace{}, fmt.Errorf("load service %s/%s of the rendered tree: %w", inventory.Module, unit.Name, err)
			}
			service = loaded
		}
		workloads, err := renderedWorkloads(unitDir, env.Name)
		if err != nil {
			return cellfile.Namespace{}, fmt.Errorf("unit %s/%s: %w", inventory.Module, unit.Name, err)
		}
		if len(workloads) == 0 {
			return cellfile.Namespace{}, fmt.Errorf("unit %s/%s renders no workload", inventory.Module, unit.Name)
		}
		manifests, err := overlayManifests(filepath.Join(unitDir, "overlays", env.Name))
		if err != nil {
			return cellfile.Namespace{}, fmt.Errorf("unit %s/%s: %w", inventory.Module, unit.Name, err)
		}
		servicePorts, containerPorts := unitPorts(unit)
		grant, _ := env.CellWorkload(inventory.Module, unit.Name)
		// A service whose render includes a non-serving workload (an agent's
		// bootstrap Job: a store's role creation, a warehouse's migration)
		// reaches the service it bootstraps under that Job's own identity,
		// and the mesh admits only the callers the cell declares. So such a
		// service declares itself a consumer of every endpoint it serves:
		// the reader resolves the reference to every workload running the
		// service, the Job included, and admits exactly those identities.
		// Nothing widens -- the Job is the service's own pod -- and nothing
		// is assumed about which endpoint it dials.
		selfConsuming := slices.ContainsFunc(workloads, func(w renderedWorkload) bool { return !servingKind(w.Kind) })
		for index := range workloads {
			rendered := &workloads[index]
			selector, err := podSelector(manifests, rendered.Name, rendered.Kind)
			if err != nil {
				return cellfile.Namespace{}, fmt.Errorf("unit %s/%s: %w", inventory.Module, unit.Name, err)
			}
			if rendered.ServiceAccount == deliveryServiceAccount {
				return cellfile.Namespace{}, fmt.Errorf("unit %s/%s workload %s runs as the %q ServiceAccount, which is reserved for the delivery Job", inventory.Module, unit.Name, rendered.Name, deliveryServiceAccount)
			}
			// The carrier is declared by its labels; a workload selected by
			// the same label would be admitted as the carrier.
			if _, carrier := selector[deliveryLabel]; carrier {
				return cellfile.Namespace{}, fmt.Errorf("unit %s/%s workload %s selects its pods by the %q label, which is reserved for the delivery Job", inventory.Module, unit.Name, rendered.Name, deliveryLabel)
			}
			authenticating, err := authenticatingContainer(unit.Name, rendered)
			if err != nil {
				return cellfile.Namespace{}, fmt.Errorf("unit %s/%s workload %s: %w", inventory.Module, unit.Name, rendered.Name, err)
			}
			workload := cellfile.Workload{
				Name: rendered.Name, Kind: rendered.Kind, Selector: selector,
				Service:        resources.ServiceUnique(inventory.Module, unit.Name),
				ServiceAccount: rendered.ServiceAccount,
				SPIFFEID:       env.Host.SPIFFEID(namespace.Name, rendered.ServiceAccount),
				Authenticating: authenticating.Name,
				Containers:     cellContainers(rendered.Containers),
				InitContainers: cellContainers(rendered.InitContainers),
				Release:        release,
				Bindings:       append([]string(nil), grant.Bindings...),
				CloudIdentity:  grant.CloudIdentity,
			}
			workload.Verifier = isDeliveryVerifier(env, workload.Kind, workload.Service)
			// Endpoints, their consumers and ingress belong to the workloads
			// that serve them. A service's own bootstrap Job runs its image
			// once and listens on nothing, so it declares none — a policy
			// derived from it would grant a Job the service's reachability.
			if service != nil && servingKind(workload.Kind) {
				for _, endpoint := range service.Endpoints {
					if endpoint == nil {
						continue
					}
					callers := consumers[workload.Service+"/"+endpoint.Name]
					if selfConsuming && !slices.Contains(callers, workload.Service) {
						callers = append(append([]string(nil), callers...), workload.Service)
					}
					entry := cellfile.Endpoint{
						Name: endpoint.Name, API: endpoint.API,
						Port:        int(containerPorts[endpoint.Name]),
						ServicePort: int(servicePorts[endpoint.Name]),
						Consumers:   callers,
						Presenter:   slices.Contains(grant.Presenter, endpoint.Name),
					}
					workload.Endpoints = append(workload.Endpoints, entry)
				}
				sort.Slice(workload.Endpoints, func(i, j int) bool { return workload.Endpoints[i].Name < workload.Endpoints[j].Name })
				// A presenter endpoint the service does not declare would be a
				// protection the cell never builds and a listener that refuses
				// every caller: refused here, by name.
				for _, name := range grant.Presenter {
					if !slices.ContainsFunc(workload.Endpoints, func(e cellfile.Endpoint) bool { return e.Name == name }) {
						return cellfile.Namespace{}, fmt.Errorf("unit %s/%s: the environment declares presenter endpoint %q, which the service does not declare", inventory.Module, unit.Name, name)
					}
				}
				// One ingress entry per endpoint, every host once: the
				// environment may route one endpoint through several
				// declarations, and a count in a cell is a count a reader
				// trusts. A route names its service module-qualified.
				routes, err := ingressRoutes(env, service, workload.Service)
				if err != nil {
					return cellfile.Namespace{}, err
				}
				for endpoint, hosts := range routes {
					sort.Strings(hosts)
					workload.Ingress = append(workload.Ingress, cellfile.Ingress{Endpoint: endpoint, Hosts: slices.Compact(hosts)})
				}
				sort.Slice(workload.Ingress, func(i, j int) bool { return workload.Ingress[i].Endpoint < workload.Ingress[j].Endpoint })
			}
			namespace.Workloads = append(namespace.Workloads, workload)
		}
	}
	sort.Slice(namespace.Workloads, func(i, j int) bool { return namespace.Workloads[i].Name < namespace.Workloads[j].Name })
	carries, err := carriesDelivery(workspace, env, inventory)
	if err != nil {
		return cellfile.Namespace{}, err
	}
	if carries {
		delivery, err := cellDelivery(env, namespace.Name)
		if err != nil {
			return cellfile.Namespace{}, fmt.Errorf("module %s: %w", inventory.Module, err)
		}
		namespace.Delivery = delivery
	}
	namespace.Egress = cellEgress(env, inventory)
	return namespace, nil
}

// ingressRoutes is every host the environment routes onto each endpoint of a
// service. A route onto an endpoint the service does not declare, or does not
// expose publicly, is refused by name — the mesh render refuses the same route
// (mesh_edges.go), judged on the manifest endpoint as core's IsExposedEndpoint
// judges the proto one; the cell must not admit an edge the mesh refuses.
func ingressRoutes(env *environments.Environment, service *resources.Service, unique string) (map[string][]string, error) {
	routes := map[string][]string{}
	for _, route := range env.Ingress {
		if route.Service != unique {
			continue
		}
		index := slices.IndexFunc(service.Endpoints, func(e *resources.Endpoint) bool { return e != nil && e.Name == route.Endpoint })
		if index < 0 {
			return nil, fmt.Errorf("ingress route %s routes onto %s endpoint %q, which the service does not declare", route.Name, unique, route.Endpoint)
		}
		if exposure := service.Endpoints[index].Exposure; exposure != resources.ExposurePublic {
			return nil, fmt.Errorf("ingress route %s routes onto %s endpoint %q, which the service declares %q, not public; a route reaches only an endpoint its module exposes", route.Name, unique, route.Endpoint, exposure)
		}
		routes[route.Endpoint] = append(routes[route.Endpoint], route.Hosts...)
	}
	return routes, nil
}

// cellEgress is what each service of the module declares it reaches outside
// the cell: the environment's egress hosts and a managed service's CIDRs.
func cellEgress(env *environments.Environment, inventory *Inventory) []cellfile.Egress {
	var out []cellfile.Egress
	for index := range inventory.Units {
		unit := &inventory.Units[index]
		if unit.Kind != UnitKindService {
			continue
		}
		egress := cellfile.Egress{Service: resources.ServiceUnique(inventory.Module, unit.Name)}
		for _, host := range env.EgressHosts(inventory.Module, unit.Name) {
			egress.Hosts = append(egress.Hosts, cellfile.EgressHost{Name: host.Name, Port: host.Port})
		}
		if managed, declared := env.ManagedService(inventory.Module, unit.Name); declared {
			egress.CIDRs = append([]string(nil), managed.EgressCIDRs...)
		}
		if len(egress.Hosts) == 0 && len(egress.CIDRs) == 0 {
			continue
		}
		out = append(out, egress)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

func cellContainers(containers []renderedContainer) []cellfile.Container {
	out := make([]cellfile.Container, 0, len(containers))
	for _, container := range containers {
		out = append(out, cellfile.Container{Name: container.Name, Image: cellfile.Image{Repository: container.Repository, Digest: container.Digest}})
	}
	return out
}

// isDeliveryVerifier marks the serving workload of the host's delivery
// endpoint: the one the cell's verifier RBAC and carrier allows are derived
// for.
func isDeliveryVerifier(env *environments.Environment, kind, service string) bool {
	if env == nil || env.Host == nil || !servingKind(kind) {
		return false
	}
	module, name, _ := env.Host.DeliveryEndpoint()
	return service == resources.ServiceUnique(module, name)
}

// unitPorts is the in-cluster Service port and the container port of each
// endpoint a unit serves, as the render resolved them from the flow's network
// mappings and the unit's rendered pods and recorded in the inventory. An
// endpoint the render could not place in-cluster has none here, and the cell's
// consumer says so if a policy needs one.
func unitPorts(unit *InventoryUnit) (servicePorts, containerPorts map[string]uint32) {
	servicePorts, containerPorts = map[string]uint32{}, map[string]uint32{}
	for _, endpoint := range unit.Endpoints {
		if endpoint.Port > 0 {
			servicePorts[endpoint.Name] = endpoint.Port
		}
		if endpoint.TargetPort > 0 {
			containerPorts[endpoint.Name] = endpoint.TargetPort
		}
	}
	return servicePorts, containerPorts
}

// podSelector is the exact label set a policy selects a workload's pods by:
// the controller's matchLabels for a serving kind, the pod template's labels
// for a Job or CronJob. A selector the cell cannot carry exactly is refused.
func podSelector(manifests []manifest, name, kind string) (map[string]string, error) {
	for _, item := range manifests {
		if item.kind != kind || metadataString(item.value, "name") != name {
			continue
		}
		var labels map[string]any
		switch kind {
		case kindDeployment, kindStatefulSet, kindDaemonSet:
			spec := mapField(mapField(item.value, "spec"), "selector")
			if expressions := sliceField(spec, "matchExpressions"); len(expressions) > 0 {
				return nil, fmt.Errorf("workload %s selects its pods with matchExpressions, which the cell file cannot carry as the exact label set a policy selects by; select the pods by labels", name)
			}
			labels = mapField(spec, "matchLabels")
		default:
			labels = mapField(mapField(podTemplate(item), "metadata"), "labels")
		}
		selector := map[string]string{}
		for key, value := range labels {
			selector[key] = fmt.Sprint(value)
		}
		if len(selector) == 0 {
			return nil, fmt.Errorf("workload %s %s carries no label on its pods, so the cell cannot name the set a policy selects", kind, name)
		}
		return selector, nil
	}
	return nil, fmt.Errorf("workload %s %s is not among the rendered manifests", kind, name)
}

// carriesDelivery reports whether a delivery Job runs in the module's
// namespace: it renders a module manifest, or it hosts the delivery API and
// the composition states the environment's module envelope, which publish
// delivers from the host's namespace.
func carriesDelivery(workspace *resources.Workspace, env *environments.Environment, inventory *Inventory) (bool, error) {
	if inventory.ModuleManifestPath != "" {
		return true, nil
	}
	if !inventory.HostsDelivery || env.Host == nil || workspace == nil {
		return false, nil
	}
	return fixture.StatesGrants(workspace, env.Name)
}

// cellDelivery is the presence carrier of a module that renders a module
// manifest for a hosted environment: the Job publish writes to POST it,
// declared from deliveryCarrierOf — the rule that Job is rendered from — and
// never read off a tree. The render writes the manifest and the account the
// Job runs as; the Job itself is written only at publish, into the delivery
// repository, so a tree in the workspace never holds it, and the cell must
// declare it before publish runs for its admission to let the Job in.
func cellDelivery(env *environments.Environment, namespace string) (*cellfile.Delivery, error) {
	carrier := deliveryCarrierOf()
	repository, digest, err := splitPinnedImage(carrier.Image)
	if err != nil {
		return nil, fmt.Errorf("the delivery Job's %w", err)
	}
	return &cellfile.Delivery{
		Kind: carrier.Kind, Selector: maps.Clone(carrier.Labels), ServiceAccount: carrier.ServiceAccount,
		SPIFFEID:  env.Host.SPIFFEID(namespace, carrier.ServiceAccount),
		Container: carrier.Container, Image: cellfile.Image{Repository: repository, Digest: digest},
	}, nil
}

// endpointConsumers indexes, by `<module>/<service>/<endpoint>`, the services
// the composition declares as its callers: every service dependency with the
// endpoints it names (or all of the target's when it names none), and every
// endpoint a composed solution consumes through its manifest, attributed to
// the solution's entry service.
func endpointConsumers(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (map[string][]string, []string, error) {
	references, referencesErr := configuredEndpointReferences(ctx, workspace, env)
	if referencesErr != nil {
		return nil, nil, referencesErr
	}
	reach, reachErr := derivedRunnableReach(ctx, workspace)
	if reachErr != nil {
		return nil, nil, reachErr
	}
	consumers := map[string][]string{}
	var configured []string
	add := func(key, consumer string) {
		if !slices.Contains(consumers[key], consumer) {
			consumers[key] = append(consumers[key], consumer)
		}
	}
	for _, reference := range workspace.Modules {
		module, err := workspace.LoadModuleFromReference(ctx, reference)
		if err != nil {
			return nil, nil, fmt.Errorf("load module %s to index its dependencies: %w", reference.Name, err)
		}
		for _, serviceReference := range module.ServiceReferences {
			service, err := module.LoadServiceFromName(ctx, serviceReference.Name)
			if err != nil {
				return nil, nil, fmt.Errorf("load service %s/%s to index its dependencies: %w", module.Name, serviceReference.Name, err)
			}
			consumer := resources.ServiceUnique(module.Name, service.Name)
			for _, dependency := range service.ServiceDependencies {
				for _, key := range dependencyKeys(ctx, workspace, module.Name, dependency) {
					add(key, consumer)
				}
			}
			reads := references[consumer]
			for _, group := range service.WorkspaceConfigurationDependencies {
				reads = append(reads, references[group]...)
			}
			for _, read := range reads {
				key, err := configuredEndpoint(ctx, workspace, consumer, read)
				if err != nil {
					return nil, nil, err
				}
				if !slices.Contains(consumers[key], consumer) {
					configured = append(configured, fmt.Sprintf("cell: %s reaches %s through %s; declared by the configuration, not by a service dependency", consumer, key, read.source))
				}
				add(key, consumer)
			}
		}
	}
	gatewayEdges, gatewayErr := hostRouteConsumers(ctx, workspace, env)
	if gatewayErr != nil {
		return nil, nil, gatewayErr
	}
	for _, edge := range append(gatewayEdges, preparedReachConsumers(reach)...) {
		if slices.Contains(consumers[edge.key], edge.consumer) {
			continue
		}
		configured = append(configured, edge.why)
		add(edge.key, edge.consumer)
	}
	consumed, err := solutionrun.ConsumedEndpoints(ctx, workspace)
	if err != nil {
		return nil, nil, fmt.Errorf("index what the composed solutions consume: %w", err)
	}
	for index := range consumed {
		consumption := &consumed[index]
		module, loadErr := workspace.LoadModuleFromName(ctx, consumption.Solution)
		if loadErr != nil || module.ServiceEntry == "" {
			continue
		}
		add(consumption.Module+"/"+consumption.Service+"/"+consumption.Endpoint, resources.ServiceUnique(module.Name, module.ServiceEntry))
	}
	for key := range consumers {
		sort.Strings(consumers[key])
	}
	return consumers, configured, nil
}

// configuredReference is one ${endpoint:…} a configuration value carries, and
// the `<configuration>/<KEY>` it is read from.
type configuredReference struct{ source, reference string }

// configuredEndpointReferences reads the ${endpoint:…} references the
// environment's configurations carry, as the configuration loader provides
// them before any is resolved: per workspace configuration group, and per
// service (`<module>/<service>`) for a service's own configuration. A service
// reading one dials that endpoint, so it is a consumer of it exactly as a
// service dependency makes it one.
func configuredEndpointReferences(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (map[string][]configuredReference, error) {
	reader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if err = reader.Load(ctx, env.Runtime()); err != nil {
		return nil, fmt.Errorf("read the configurations of environment %s for the endpoints they reference: %w", env.Name, err)
	}
	references := map[string][]configuredReference{}
	for _, conf := range reader.Configurations() {
		for _, info := range conf.Infos {
			owner := conf.Origin
			if owner == resources.ConfigurationWorkspace {
				owner = info.Name
			}
			for _, value := range info.ConfigurationValues {
				for _, reference := range resources.ConfigurationValueEndpointReferences(value) {
					references[owner] = append(references[owner], configuredReference{source: info.Name + "/" + value.Key, reference: reference})
				}
			}
		}
	}
	return references, nil
}

// configuredEndpoint is the `<module>/<service>/<endpoint>` a configured
// reference names, refused, naming the service, the key and the target, when
// the composition has no such endpoint.
func configuredEndpoint(ctx context.Context, workspace *resources.Workspace, consumer string, read configuredReference) (string, error) {
	refuse := func(why string) error {
		return fmt.Errorf("service %s reads %s = ${endpoint:%s}, %s", consumer, read.source, read.reference, why)
	}
	info, err := resources.ParseEndpointReference(read.reference)
	if err != nil {
		return "", refuse(err.Error())
	}
	target, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: info.Service, Module: info.Module})
	if err != nil {
		return "", refuse(fmt.Sprintf("but the workspace composes no service %s/%s", info.Module, info.Service))
	}
	for _, endpoint := range target.Endpoints {
		if endpoint != nil && ((info.Name != "" && endpoint.Name == info.Name) || (info.Name == "" && endpoint.API == info.API)) {
			return resources.ServiceUnique(info.Module, info.Service) + "/" + endpoint.Name, nil
		}
	}
	return "", refuse(fmt.Sprintf("but service %s/%s declares no such endpoint", info.Module, info.Service))
}

const (
	workloadLabel     = "codefly.dev/workload"
	workloadKindLabel = "codefly.dev/workload-kind"
)

// labelPodTemplates gives every Job and CronJob pod template in a rendered
// tree that carries no label the two labels that name it (the workload's
// name and kind), so the cell file can carry the exact label set a policy
// selects its pods by. A serving workload selects its pods by its own
// matchLabels; a bootstrap Job an agent renders without labels would otherwise
// be a workload the cell cannot name.
func labelPodTemplates(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (filepath.Ext(path) != yamlExtension && filepath.Ext(path) != ymlExtension) {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := readWithin(root, relative)
		if err != nil {
			return err
		}
		documents, _, err := decodeYAML(relative, data)
		if err != nil {
			return fmt.Errorf("label the pod templates of %s: %w", relative, err)
		}
		changed := false
		for _, document := range documents {
			if document.kind != kindJob && document.kind != kindCronJob {
				continue
			}
			template := podTemplate(document)
			if template == nil || len(mapField(mapField(template, "metadata"), "labels")) > 0 {
				continue
			}
			metadata, _ := template["metadata"].(map[string]any)
			if metadata == nil {
				metadata = map[string]any{}
				template["metadata"] = metadata
			}
			metadata["labels"] = map[string]any{
				workloadLabel:     metadataString(document.value, "name"),
				workloadKindLabel: strings.ToLower(document.kind),
			}
			changed = true
		}
		if !changed {
			return nil
		}
		var out strings.Builder
		encoder := yaml.NewEncoder(&out)
		encoder.SetIndent(2)
		for _, document := range documents {
			if err := encoder.Encode(document.value); err != nil {
				return err
			}
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		return shared.WriteFileAtomic(ctx, path, []byte(out.String()), 0o644)
	})
}

// consumerEdge is one consumer of one endpoint, with the sentence the render
// prints to say where it came from: an edge nobody declared is reported, or
// an operator reads a cell admitting a caller no service names.
type consumerEdge struct {
	key      string
	consumer string
	why      string
}

// hostRouteConsumers is the gateway on every endpoint a composed solution
// reaches through the host (orchestration.HostRouteTargets, the derivation the
// mesh edges read). A route is one hop THROUGH the gateway, not a direct call,
// so a cell that admits only the solution's backend denies every proxied call
// with nothing in the module's log to say so.
func hostRouteConsumers(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) ([]consumerEdge, error) {
	if env.Host == nil || env.Host.Gateway == "" {
		return nil, nil
	}
	gatewayModule, _, _ := strings.Cut(env.Host.Gateway, "/")
	targets, err := orchestration.HostRouteTargets(ctx, workspace, gatewayModule)
	if err != nil {
		return nil, err
	}
	edges := make([]consumerEdge, 0, len(targets))
	for _, target := range targets {
		key := resources.ServiceUnique(target.Module, target.Service) + "/" + target.Endpoint
		edges = append(edges, consumerEdge{key: key, consumer: env.Host.Gateway,
			why: fmt.Sprintf("cell: %s reaches %s because the host routes it there; declared by the composition's consumption, not by a service dependency", env.Host.Gateway, key)})
	}
	return edges, nil
}

// preparedReachConsumers is every executor on every owner endpoint its
// prepared runnable bindings call (derivedRunnableReach). The owner publishes
// an operation and the executor installs it; neither declares a dependency on
// the other, so without this the cell admits nobody on the owner's endpoint,
// the mesh denies the call, and the executor reports only an owner that did
// not answer.
func preparedReachConsumers(reach *runnableReach) []consumerEdge {
	if reach == nil {
		return nil
	}
	var edges []consumerEdge
	for key := range reach.owners {
		for _, executor := range reach.executors {
			consumer := executor.Unique()
			edges = append(edges, consumerEdge{key: key, consumer: consumer,
				why: fmt.Sprintf("cell: %s reaches %s through the runnable bindings it installs; prepared by the composition, not declared by either service", consumer, key)})
		}
	}
	slices.SortFunc(edges, func(a, b consumerEdge) int {
		if a.key != b.key {
			return strings.Compare(a.key, b.key)
		}
		return strings.Compare(a.consumer, b.consumer)
	})
	return edges
}

// dependencyKeys is the `<module>/<service>/<endpoint>` of every endpoint one
// declared service dependency reaches: the endpoints it names, or all of the
// target's when it names none. A dependency on a service the workspace does
// not compose reaches nothing here; the composition refuses it elsewhere.
func dependencyKeys(ctx context.Context, workspace *resources.Workspace, module string, dependency *resources.ServiceDependency) []string {
	if dependency == nil || dependency.Name == "" {
		return nil
	}
	targetModule := dependency.Module
	if targetModule == "" {
		targetModule = module
	}
	target, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: dependency.Name, Module: targetModule})
	if err != nil {
		return nil
	}
	var names []string
	for _, endpoint := range dependency.Endpoints {
		if endpoint != nil && endpoint.Name != "" {
			names = append(names, endpoint.Name)
		}
	}
	if len(names) == 0 {
		for _, endpoint := range target.Endpoints {
			if endpoint != nil {
				names = append(names, endpoint.Name)
			}
		}
	}
	keys := make([]string, 0, len(names))
	for _, name := range names {
		keys = append(keys, resources.ServiceUnique(targetModule, target.Name)+"/"+name)
	}
	return keys
}
