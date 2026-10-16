package gitops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
	"github.com/codefly-dev/cli/pkg/solutionrun"
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
	// Source reports whether the file names the commit it was emitted from.
	// It does not when the environment declares no gitops repository or the
	// workspace is not a git checkout; a consumer binding a delivery to an
	// exact revision needs it.
	Source bool
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
	if workspace == nil || env == nil {
		return CellResult{}, errors.New("a cell is rendered for a workspace and an environment")
	}
	if env.Host == nil {
		return CellResult{}, fmt.Errorf("environment %s declares no host, so it has no cell to describe", env.Name)
	}
	modulesDir := filepath.Dir(moduleRenderDestination(workspace, "module"))
	entries, err := os.ReadDir(modulesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return CellResult{}, fmt.Errorf("no module has been rendered under %s", modulesDir)
	}
	if err != nil {
		return CellResult{}, fmt.Errorf("list rendered modules: %w", err)
	}
	consumers, err := endpointConsumers(ctx, workspace)
	if err != nil {
		return CellResult{}, err
	}
	file := cellfile.File{
		Schema: cellfile.Schema, Environment: env.Name,
		Coordinate: env.Host.Coordinate, Component: env.Host.Component, Domain: env.Host.Domain, TrustDomain: env.Host.TrustDomain,
		Source: cellSource(ctx, workspace, env),
	}
	result := CellResult{Source: file.Source != nil}
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
			return CellResult{}, fmt.Errorf("the render of module %s cannot be read, so the cell file cannot describe what it delivers; render %s again for %s: %w", entry.Name(), entry.Name(), env.Name, loadErr)
		}
		if inventory.Environment != env.Name {
			result.Skipped = append(result.Skipped, inventory.Module)
			continue
		}
		namespace, namespaceErr := cellNamespace(ctx, workspace, env, tree, &inventory, consumers)
		if namespaceErr != nil {
			return CellResult{}, namespaceErr
		}
		file.Namespaces = append(file.Namespaces, namespace)
		result.Modules = append(result.Modules, inventory.Module)
	}
	if len(file.Namespaces) == 0 {
		return CellResult{}, fmt.Errorf("no module is rendered for environment %s under %s", env.Name, modulesDir)
	}
	sort.Slice(file.Namespaces, func(i, j int) bool { return file.Namespaces[i].Name < file.Namespaces[j].Name })
	sort.Strings(result.Modules)
	sort.Strings(result.Skipped)
	body, err := file.Encode()
	if err != nil {
		return CellResult{}, fmt.Errorf("the rendered cell cannot be written: %w", err)
	}
	if result.Path, err = CellPath(workspace, env); err != nil {
		return CellResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(result.Path), 0o755); err != nil {
		return CellResult{}, fmt.Errorf("create the cell directory: %w", err)
	}
	if err := os.WriteFile(result.Path, body, 0o600); err != nil {
		return CellResult{}, fmt.Errorf("write the cell file: %w", err)
	}
	return result, nil
}

// cellSource is the repository, publication branch and exact commit the
// inventory is emitted from: the environment's gitops declaration and the
// workspace checkout's HEAD. Nil when either is missing, which the result
// reports rather than inventing a revision.
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
	for _, unit := range inventory.Units {
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
		servicePorts, containerPorts := edgePorts(inventory.MeshEdges, inventory.Module, unit.Name)
		grant, _ := env.CellWorkload(inventory.Module, unit.Name)
		for index := range workloads {
			rendered := &workloads[index]
			selector, err := podSelector(manifests, rendered.Name, rendered.Kind)
			if err != nil {
				return cellfile.Namespace{}, fmt.Errorf("unit %s/%s: %w", inventory.Module, unit.Name, err)
			}
			if rendered.ServiceAccount == deliveryServiceAccount {
				return cellfile.Namespace{}, fmt.Errorf("unit %s/%s workload %s runs as the %q ServiceAccount, which is reserved for the delivery Job", inventory.Module, unit.Name, rendered.Name, deliveryServiceAccount)
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
					entry := cellfile.Endpoint{
						Name: endpoint.Name, API: endpoint.API,
						Port:        int(containerPorts[endpoint.Name]),
						ServicePort: int(servicePorts[endpoint.Name]),
						Consumers:   consumers[workload.Service+"/"+endpoint.Name],
					}
					workload.Endpoints = append(workload.Endpoints, entry)
				}
				sort.Slice(workload.Endpoints, func(i, j int) bool { return workload.Endpoints[i].Name < workload.Endpoints[j].Name })
				// One ingress entry per endpoint, every host once: the
				// environment may route one endpoint through several
				// declarations, and a count in a cell is a count a reader
				// trusts. A route names its service module-qualified.
				routes := map[string][]string{}
				for _, route := range env.Ingress {
					if route.Service == workload.Service {
						routes[route.Endpoint] = append(routes[route.Endpoint], route.Hosts...)
					}
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
	delivery, err := renderedDelivery(tree, env, namespace.Name)
	if err != nil {
		return cellfile.Namespace{}, fmt.Errorf("module %s: %w", inventory.Module, err)
	}
	namespace.Delivery = delivery
	for _, unit := range inventory.Units {
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
		namespace.Egress = append(namespace.Egress, egress)
	}
	sort.Slice(namespace.Egress, func(i, j int) bool { return namespace.Egress[i].Service < namespace.Egress[j].Service })
	return namespace, nil
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

// edgePorts is the in-cluster Service port and the container port of each
// endpoint a module's render recorded a mesh edge for: the ports the render
// derived from the flow's network mappings and the unit's rendered pods
// (deriveMeshEdges), read back from the committed inventory. An endpoint
// nothing consumes has no edge and no port here; a policy needs none for it.
func edgePorts(edges *MeshEdges, module, service string) (servicePorts, containerPorts map[string]uint32) {
	servicePorts, containerPorts = map[string]uint32{}, map[string]uint32{}
	if edges == nil {
		return servicePorts, containerPorts
	}
	for _, edge := range edges.Edges {
		if edge.Module != module || edge.Service != service || edge.Endpoint == "" {
			continue
		}
		if edge.Port > 0 {
			servicePorts[edge.Endpoint] = edge.Port
		}
		if edge.TargetPort > 0 {
			containerPorts[edge.Endpoint] = edge.TargetPort
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

// renderedDelivery is the presence carrier the render wrote for a module: the
// Job labelled as the manifest delivery, read off the tree so the cell
// declares exactly what runs. Nil when the module renders none.
func renderedDelivery(tree string, env *environments.Environment, namespace string) (*cellfile.Delivery, error) {
	var found *cellfile.Delivery
	err := filepath.WalkDir(tree, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || (filepath.Ext(path) != yamlExtension && filepath.Ext(path) != ymlExtension) {
			return walkErr
		}
		relative, err := filepath.Rel(tree, path)
		if err != nil {
			return err
		}
		data, err := readWithin(tree, relative)
		if err != nil {
			return err
		}
		documents, _, err := decodeYAML(relative, data)
		if err != nil {
			return nil
		}
		for _, document := range documents {
			if document.kind != kindJob && document.kind != kindCronJob {
				continue
			}
			labels := mapField(mapField(document.value, "metadata"), "labels")
			if kind, _ := labels[deliveryLabel].(string); kind != deliveryManifest {
				continue
			}
			if found != nil {
				return fmt.Errorf("the tree renders two delivery Jobs (%s and %s); the cell declares one carrier per namespace", found.Container, metadataString(document.value, "name"))
			}
			spec, ok := podSpec(document)
			if !ok {
				continue
			}
			template := mapField(mapField(podTemplate(document), "metadata"), "labels")
			selector := map[string]string{}
			for key, value := range template {
				selector[key] = fmt.Sprint(value)
			}
			containers := sliceField(spec, "containers")
			if len(containers) != 1 {
				return fmt.Errorf("the delivery Job %s runs %d containers; the cell declares the one that delivers", metadataString(document.value, "name"), len(containers))
			}
			container, _ := containers[0].(map[string]any)
			name, _ := container["name"].(string)
			image, _ := container["image"].(string)
			repository, digest, pinned := strings.Cut(image, "@")
			if !pinned {
				return fmt.Errorf("the delivery Job %s image %q is not pinned by digest", metadataString(document.value, "name"), image)
			}
			account, _ := spec["serviceAccountName"].(string)
			if account == "" {
				account = defaultServiceAccount
			}
			found = &cellfile.Delivery{
				Kind: document.kind, Selector: selector, ServiceAccount: account,
				SPIFFEID:  env.Host.SPIFFEID(namespace, account),
				Container: name, Image: cellfile.Image{Repository: repository, Digest: digest},
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// endpointConsumers indexes, by `<module>/<service>/<endpoint>`, the services
// the composition declares as its callers: every service dependency with the
// endpoints it names (or all of the target's when it names none), and every
// endpoint a composed solution consumes through its manifest, attributed to
// the solution's entry service.
func endpointConsumers(ctx context.Context, workspace *resources.Workspace) (map[string][]string, error) {
	consumers := map[string][]string{}
	add := func(key, consumer string) {
		if !slices.Contains(consumers[key], consumer) {
			consumers[key] = append(consumers[key], consumer)
		}
	}
	for _, reference := range workspace.Modules {
		module, err := workspace.LoadModuleFromReference(ctx, reference)
		if err != nil {
			return nil, fmt.Errorf("load module %s to index its dependencies: %w", reference.Name, err)
		}
		for _, serviceReference := range module.ServiceReferences {
			service, err := module.LoadServiceFromName(ctx, serviceReference.Name)
			if err != nil {
				return nil, fmt.Errorf("load service %s/%s to index its dependencies: %w", module.Name, serviceReference.Name, err)
			}
			consumer := resources.ServiceUnique(module.Name, service.Name)
			for _, dependency := range service.ServiceDependencies {
				if dependency == nil || dependency.Name == "" {
					continue
				}
				targetModule := dependency.Module
				if targetModule == "" {
					targetModule = module.Name
				}
				target, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: dependency.Name, Module: targetModule})
				if err != nil {
					continue
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
				for _, name := range names {
					add(resources.ServiceUnique(targetModule, target.Name)+"/"+name, consumer)
				}
			}
		}
	}
	consumed, err := solutionrun.ConsumedEndpoints(ctx, workspace)
	if err != nil {
		return nil, fmt.Errorf("index what the composed solutions consume: %w", err)
	}
	for _, consumption := range consumed {
		module, err := workspace.LoadModuleFromName(ctx, consumption.Solution)
		if err != nil || module.ServiceEntry == "" {
			continue
		}
		add(consumption.Module+"/"+consumption.Service+"/"+consumption.Endpoint, resources.ServiceUnique(module.Name, module.ServiceEntry))
	}
	for key := range consumers {
		sort.Strings(consumers[key])
	}
	return consumers, nil
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
			return nil
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
