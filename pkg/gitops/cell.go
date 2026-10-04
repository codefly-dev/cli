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

	"github.com/codefly-dev/core/solutionhost/cell"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// --- The cell file ---
//
// A cell is one deployment host and everything delivered to it. The platform
// derives its mesh policy — which workload may reach which endpoint, under which
// identity, on which port — from an inventory of what is delivered, rather than
// from a hand-written set that drifts the moment a module is added. The cell
// file is that inventory, emitted by the render from the composition and the
// trees it has rendered, for one environment.
//
// It is a build-time inventory, regenerated whole on every render, so absence is
// correct: a workload not in the file is not delivered. That is the OPPOSITE of
// the presence document's rule, where removal is a generation and never an
// absence, and the two must not be confused — the cell file carries no
// generation, no ownership domain and no tombstone.
//
// It cannot live in a module's owned tree: the promotable ruleset refuses any
// YAML there that is not a Kubernetes manifest or a Kustomization. It lives
// beside the module trees, under deployments/cells/<environment>/, and publish
// copies it into the delivery repository outside every module path, where the
// platform's derivation reads it.

const (
	// defaultServiceAccount is the account a pod template naming none runs as:
	// shared by every such pod of the namespace, so never one presence names.
	defaultServiceAccount = "default"
	cellsDir              = "cells"
	// cellRecordFile is the cell record a hosted render writes at its tree's
	// root: the module's namespace entry as rendered, read by publish and by
	// the whole-workspace cell render.
	cellRecordFile = cell.FileName
)

// renderedWorkload is a cell workload as the render reads it off the rendered
// manifests, with the one thing the render alone needs and the cell never
// carries: the projected token volumes each container mounts, by container
// name, read so the render can refuse a token a sidecar could present.
type renderedWorkload struct {
	cell.Workload
	tokenMounts map[string][]tokenMount
}

// tokenMount is one projected ServiceAccount token volume carrying an
// explicit audience, as a container mounts it.
type tokenMount struct {
	volume   string
	audience string
}

// CellResult reports what a cell render wrote and what it left out.
type CellResult struct {
	// Path is the cell file written.
	Path string
	// Modules are the modules whose rendered trees the file covers.
	Modules []string
	// Skipped names module trees under deployments/modules that were rendered
	// for another environment, and so describe another cell.
	Skipped []string
}

// servesTraffic reports whether a workload of this kind is one a service's
// endpoints are served from.
func servesTraffic(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		return true
	}
	return false
}

// cellPath is the cell file of one environment under the workspace's
// deployments directory.
func cellPath(workspaceDir, environment string) string {
	return filepath.Join(workspaceDir, "deployments", cellsDir, environment, cell.FileName)
}

// RenderCell derives the environment's cell file from every module tree
// rendered under the workspace's deployments directory for that environment,
// and the composition that produced them, and writes it. It is called after a
// module render, so the file always describes the trees as they stand.
func RenderCell(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (CellResult, error) {
	if workspace == nil || env == nil {
		return CellResult{}, fmt.Errorf("a cell is rendered for a workspace and an environment")
	}
	modulesDir := filepath.Join(workspace.Dir(), "deployments", "modules")
	entries, err := os.ReadDir(modulesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return CellResult{}, fmt.Errorf("no module has been rendered under %s", modulesDir)
	}
	if err != nil {
		return CellResult{}, fmt.Errorf("list rendered modules: %w", err)
	}
	file := cell.File{Schema: cell.SchemaV1, Environment: env.Name}
	if env.Host != nil {
		file.Coordinate, file.Component, file.Domain, file.TrustDomain = env.Host.Coordinate, env.Host.Component, env.Host.Domain, env.Host.TrustDomain
	}
	consumers, err := endpointConsumers(ctx, workspace)
	if err != nil {
		return CellResult{}, err
	}
	result := CellResult{}
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
			return CellResult{}, fmt.Errorf("read the render of module %s: %w", entry.Name(), loadErr)
		}
		if inventory.Environment != env.Name {
			result.Skipped = append(result.Skipped, inventory.Module)
			continue
		}
		record, recordErr := readCellRecord(tree)
		if recordErr != nil {
			return CellResult{}, fmt.Errorf("module %s: %w", inventory.Module, recordErr)
		}
		var namespace cell.Namespace
		if record != nil {
			// A hosted render recorded its entry with the tree: the cell carries
			// it as rendered, not a derivation over the composition as it stands
			// now, which is what publish delivers.
			if record.Namespaces[0].Module != inventory.Module {
				return CellResult{}, fmt.Errorf("the cell record of the tree of module %s describes module %s", inventory.Module, record.Namespaces[0].Module)
			}
			namespace = record.Namespaces[0]
		} else {
			var namespaceErr error
			if namespace, namespaceErr = cellNamespace(ctx, workspace, env, tree, &inventory, consumers); namespaceErr != nil {
				return CellResult{}, namespaceErr
			}
		}
		file.Namespaces = append(file.Namespaces, namespace)
		result.Modules = append(result.Modules, inventory.Module)
	}
	sort.Slice(file.Namespaces, func(i, j int) bool { return file.Namespaces[i].Name < file.Namespaces[j].Name })
	sort.Strings(result.Modules)
	sort.Strings(result.Skipped)
	// The writer is held to the model it writes: a cell this render produced
	// that core would refuse is a render bug, caught here rather than at the
	// loader.
	if err = file.Validate(); err != nil {
		return CellResult{}, fmt.Errorf("the rendered cell does not validate: %w", err)
	}
	body, err := yaml.Marshal(file)
	if err != nil {
		return CellResult{}, fmt.Errorf("encode cell file: %w", err)
	}
	result.Path = cellPath(workspace.Dir(), env.Name)
	if err := os.MkdirAll(filepath.Dir(result.Path), 0o755); err != nil {
		return CellResult{}, fmt.Errorf("create cell directory: %w", err)
	}
	if err := os.WriteFile(result.Path, body, 0o600); err != nil {
		return CellResult{}, fmt.Errorf("write cell file: %w", err)
	}
	return result, nil
}

// cellNamespace derives one module's namespace entry from its rendered tree.
func cellNamespace(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, tree string, inventory *Inventory, consumers map[string][]string) (cell.Namespace, error) {
	namespace := cell.Namespace{Name: inventory.Namespace, Module: inventory.Module}
	var release *cell.Release
	if inventory.Package != nil {
		publisher, name, found := strings.Cut(inventory.Package.ID, "/")
		if found {
			release = &cell.Release{Publisher: publisher, Name: name, Version: inventory.Package.Version}
		}
	}
	for _, unit := range inventory.Units {
		// Every pod-producing unit is inventoried: a managed service's
		// bootstrap bundle runs a Job the closed admission set would refuse
		// unless declared, and a solution unit runs the solution. What differs
		// is only that a managed unit declares no endpoints of its own.
		if unit.Path == "" {
			continue
		}
		var service *resources.Service
		var ports map[string]uint32
		if !unit.Managed {
			loaded, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: unit.Name, Module: inventory.Module})
			if err != nil {
				return cell.Namespace{}, fmt.Errorf("load service %s/%s of the rendered tree: %w", inventory.Module, unit.Name, err)
			}
			service = loaded
			if ports, err = declaredEndpointPorts(service); err != nil {
				return cell.Namespace{}, err
			}
		}
		digest, err := unitDigest(tree, unit.Path)
		if err != nil {
			return cell.Namespace{}, err
		}
		workloads, err := renderedWorkloads(filepath.Join(tree, filepath.FromSlash(unit.Path)), env.Name)
		if err != nil {
			return cell.Namespace{}, fmt.Errorf("unit %s/%s: %w", inventory.Module, unit.Name, err)
		}
		if len(workloads) == 0 {
			return cell.Namespace{}, fmt.Errorf("unit %s/%s renders no workload", inventory.Module, unit.Name)
		}
		grant, _ := env.CellWorkload(inventory.Module, unit.Name)
		for index := range workloads {
			workload := &workloads[index]
			workload.Service = resources.ServiceUnique(inventory.Module, unit.Name)
			workload.Artifact = cell.Artifact{Name: unit.Name, Digest: digest}
			workload.Release = release
			if workload.ServiceAccount == deliveryServiceAccount {
				return cell.Namespace{}, fmt.Errorf("unit %s/%s workload %s runs as the %q ServiceAccount, which is reserved for the delivery Job", inventory.Module, unit.Name, workload.Name, deliveryServiceAccount)
			}
			authenticating, _, err := authenticatingContainer(unit.Name, workload)
			if err != nil {
				return cell.Namespace{}, fmt.Errorf("unit %s/%s workload %s: %w", inventory.Module, unit.Name, workload.Name, err)
			}
			workload.Authenticating = authenticating.Name
			workload.Bindings = append([]string(nil), grant.Bindings...)
			workload.CloudIdentity = grant.CloudIdentity
			if env.Host != nil {
				workload.SPIFFEID = env.Host.SPIFFEID(inventory.Namespace, workload.ServiceAccount)
				workload.Verifier = isDeliveryVerifier(env, &workload.Workload)
			}
			// Endpoints, their consumers and ingress belong to the workloads
			// that serve them. A service's own bootstrap Job or CronJob runs
			// its image once and listens on nothing, so it declares none —
			// a policy derived from it would grant a Job the service's
			// reachability.
			if service == nil || !servesTraffic(workload.Kind) {
				namespace.Workloads = append(namespace.Workloads, workload.Workload)
				continue
			}
			for _, endpoint := range service.Endpoints {
				if endpoint == nil {
					continue
				}
				workload.Endpoints = append(workload.Endpoints, cell.Endpoint{
					Name: endpoint.Name, API: endpoint.API, Port: ports[endpoint.Name],
					Visibility: endpoint.Visibility, AllowModules: append([]string(nil), endpoint.AllowModules...),
					Consumers: consumers[workload.Service+"/"+endpoint.Name],
				})
			}
			sort.Slice(workload.Endpoints, func(i, j int) bool { return workload.Endpoints[i].Name < workload.Endpoints[j].Name })
			// An ingress route names its service module-qualified. Matched on
			// the bare name too, every module's "api" would inherit another
			// module's public hosts.
			for _, route := range env.Ingress {
				if route.Service == workload.Service {
					workload.Ingress = append(workload.Ingress, cell.Ingress{Endpoint: route.Endpoint, Hosts: append([]string(nil), route.Hosts...)})
				}
			}
			sort.Slice(workload.Ingress, func(i, j int) bool { return workload.Ingress[i].Endpoint < workload.Ingress[j].Endpoint })
			namespace.Workloads = append(namespace.Workloads, workload.Workload)
		}
	}
	sort.Slice(namespace.Workloads, func(i, j int) bool { return namespace.Workloads[i].Name < namespace.Workloads[j].Name })
	if env.Host != nil && inventory.SolutionHostBindingPath != "" {
		namespace.Delivery = deliveryDeclaration(env, inventory.Namespace)
	}
	for _, unit := range inventory.Units {
		if unit.Kind != UnitKindService {
			continue
		}
		egress := cell.Egress{Service: resources.ServiceUnique(inventory.Module, unit.Name)}
		for _, host := range env.EgressHosts(inventory.Module, unit.Name) {
			egress.Hosts = append(egress.Hosts, cell.EgressHost{Name: host.Name, Port: host.Port})
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

// deliveryDeclaration is the delivery Job as the cell declares it for a
// namespace: the one workload the closed admission set admits beyond what the
// tree renders. Publish declares it from the settled inventory — a render
// cannot know the tombstones a publish synthesizes and delivers.
func deliveryDeclaration(env *environments.Environment, namespace string) *cell.Delivery {
	repository, digest, _ := strings.Cut(deliveryImage, "@")
	return &cell.Delivery{
		Kind:           kindJob,
		Selector:       map[string]string{managedByLabel: managedByCodefly, deliveryLabel: deliveryPresence},
		ServiceAccount: deliveryServiceAccount,
		SPIFFEID:       env.Host.SPIFFEID(namespace, deliveryServiceAccount),
		Container:      deliveryContainerName,
		Image:          cell.Image{Repository: repository, Digest: digest},
	}
}

// renderCellRecord writes a hosted render's cell record into its tree: the
// module's namespace entry — workloads, images, selectors, accounts,
// identities, artifacts and release read off the tree; endpoints, ports,
// ingress, bindings, cloud identity and egress as the composition declares
// them NOW — and, on the options, the module's outgoing consumer edges, which
// the inventory carries. Publish and rollback derive the delivered cell from
// this record and the settled inventory, never from the composition as it
// stands later. A packaged solution has no derivation (RenderSolution renders
// no composition service this reader can hold a workload to), so its record
// is the entry the workspace's cell file carries for it, hand-written before
// the render — the carried limitation the doc names.
func renderCellRecord(ctx context.Context, tree string, opts *RenderOptions) error {
	if opts.Host == nil || opts.Composition == nil || opts.Target == nil {
		return nil
	}
	head := inventoryHead(opts)
	graph, err := endpointConsumers(ctx, opts.Composition)
	if err != nil {
		return err
	}
	var namespace cell.Namespace
	if packagedUnits(head.Units) {
		entry, found, readErr := workspaceCellEntry(opts.Composition, opts.Target, opts.Module)
		if readErr != nil {
			return readErr
		}
		if !found {
			// Nothing to record: a hosted publish of this tree is refused by
			// name until the entry is written and the solution rendered again.
			return nil
		}
		namespace = entry
	} else if namespace, err = cellNamespace(ctx, opts.Composition, opts.Target, tree, &head, graph); err != nil {
		return fmt.Errorf("derive the cell record of module %s: %w", opts.Module, err)
	}
	record := cellFileFor(opts.Target, &namespace)
	if err = record.Validate(); err != nil {
		return fmt.Errorf("the cell record of module %s does not validate: %w", opts.Module, err)
	}
	body, err := yaml.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode the cell record: %w", err)
	}
	if err = os.WriteFile(filepath.Join(tree, cellRecordFile), body, 0o600); err != nil {
		return fmt.Errorf("write the cell record: %w", err)
	}
	opts.ConsumedEndpoints = moduleEdges(graph, opts.Module)
	return nil
}

// cellFileFor is a cell file carrying one module's entry under the host the
// environment declares: the shape of a record and of a contribution alike.
func cellFileFor(env *environments.Environment, namespace *cell.Namespace) *cell.File {
	file := &cell.File{Schema: cell.SchemaV1, Environment: env.Name, Namespaces: []cell.Namespace{*namespace}}
	if env.Host != nil {
		file.Coordinate, file.Component, file.Domain, file.TrustDomain = env.Host.Coordinate, env.Host.Component, env.Host.Domain, env.Host.TrustDomain
	}
	return file
}

// packagedUnits reports whether an inventory's units are a packaged solution's
// — a unit of the solution kind, run by an executor rather than rendered from
// composition services.
func packagedUnits(units []InventoryUnit) bool {
	for _, unit := range units {
		if unit.Kind == UnitKindSolution {
			return true
		}
	}
	return false
}

// workspaceCellEntry is the entry the workspace's cell file carries for a
// module, read through core's reader; found is false when the file or the
// entry is absent.
func workspaceCellEntry(workspace *resources.Workspace, env *environments.Environment, module string) (cell.Namespace, bool, error) {
	source := cellPath(workspace.Dir(), env.Name)
	data, err := readWithin(filepath.Dir(source), filepath.Base(source))
	if errors.Is(err, fs.ErrNotExist) {
		return cell.Namespace{}, false, nil
	}
	if err != nil {
		return cell.Namespace{}, false, fmt.Errorf("read the cell file %s: %w", source, err)
	}
	file, err := readCellFile(data)
	if err != nil {
		return cell.Namespace{}, false, err
	}
	for index := range file.Namespaces {
		if file.Namespaces[index].Module == module {
			return file.Namespaces[index], true, nil
		}
	}
	return cell.Namespace{}, false, nil
}

// readCellRecord reads the cell record a hosted render wrote into its tree,
// through core's reader; nil when the tree carries none.
func readCellRecord(tree string) (*cell.File, error) {
	data, err := readWithin(tree, cellRecordFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the cell record: %w", err)
	}
	record, err := readCellFile(data)
	if err != nil {
		return nil, fmt.Errorf("the cell record: %w", err)
	}
	if len(record.Namespaces) != 1 {
		return nil, fmt.Errorf("the cell record carries %d namespace entries, not one module's", len(record.Namespaces))
	}
	return record, nil
}

// isDeliveryVerifier reports whether a workload is a serving workload of the
// service the environment's host block names as the delivery API. A bootstrap
// Job of that service runs its own image and verifies nothing, so only the
// kinds that serve are marked.
func isDeliveryVerifier(env *environments.Environment, workload *cell.Workload) bool {
	if env == nil || env.Host == nil {
		return false
	}
	switch workload.Kind {
	case kindDeployment, kindStatefulSet, kindDaemonSet:
	default:
		return false
	}
	module, service, _ := env.Host.DeliveryEndpoint()
	return workload.Service == resources.ServiceUnique(module, service)
}

// renderedWorkloads reads the pod-template-bearing workloads out of a unit's
// built overlay: their names, the account they run as, and their containers'
// pinned images.
func renderedWorkloads(unitDir, environment string) ([]renderedWorkload, error) {
	overlay := filepath.Join(unitDir, "overlays", environment)
	manifests, err := overlayManifests(overlay)
	if err != nil {
		return nil, err
	}
	var workloads []renderedWorkload
	for _, item := range manifests {
		switch item.kind {
		case kindDeployment, kindStatefulSet, kindDaemonSet, kindJob, kindCronJob:
		default:
			continue
		}
		spec, ok := podSpec(item)
		if !ok {
			continue
		}
		selector, err := podSelector(item)
		if err != nil {
			return nil, err
		}
		workload := renderedWorkload{Workload: cell.Workload{Name: metadataString(item.value, "name"), Kind: item.kind, Selector: selector}, tokenMounts: map[string][]tokenMount{}}
		workload.ServiceAccount, _ = spec["serviceAccountName"].(string)
		if workload.ServiceAccount == "" {
			workload.ServiceAccount = defaultServiceAccount
		}
		tokens := audienceTokenVolumes(sliceField(spec, "volumes"))
		containers, err := renderedContainers(workload.Name, sliceField(spec, "containers"), tokens, workload.tokenMounts)
		if err != nil {
			return nil, err
		}
		workload.Containers = containers
		if workload.InitContainers, err = renderedContainers(workload.Name, sliceField(spec, "initContainers"), tokens, workload.tokenMounts); err != nil {
			return nil, err
		}
		workloads = append(workloads, workload)
	}
	return workloads, nil
}

// audienceTokenVolumes indexes a pod template's projected volumes that carry a
// ServiceAccount token minted for an explicit audience, by volume name. The
// token the ServiceAccount admission plugin injects (kube-api-access-…) is a
// projected token volume too, mounted into every container, and names no
// audience — keying on the audience is what tells the two apart, and it is
// what the cell's admission rule keys on.
func audienceTokenVolumes(raw []any) map[string]string {
	audiences := map[string]string{}
	for _, entry := range raw {
		volume, _ := entry.(map[string]any)
		name, _ := volume["name"].(string)
		for _, source := range sliceField(mapField(volume, "projected"), "sources") {
			projected, _ := source.(map[string]any)
			if audience, _ := mapField(projected, "serviceAccountToken")["audience"].(string); audience != "" && name != "" {
				audiences[name] = audience
			}
		}
	}
	return audiences
}

// renderedContainers reads a pod template's containers, their pinned images
// and the audience-bearing token volumes each mounts.
func renderedContainers(workload string, raw []any, tokens map[string]string, mounts map[string][]tokenMount) ([]cell.Container, error) {
	var containers []cell.Container
	for _, entry := range raw {
		container, _ := entry.(map[string]any)
		name, _ := container["name"].(string)
		image, _ := container["image"].(string)
		repository, digest, pinned := strings.Cut(image, "@")
		if !pinned {
			return nil, fmt.Errorf("workload %s container %s image %q is not pinned by digest", workload, name, image)
		}
		// The repository is the name only: a tag beside a digest would give one
		// container two answers about what it runs.
		if at := strings.LastIndex(repository, ":"); at > strings.LastIndex(repository, "/") {
			repository = repository[:at]
		}
		rendered := cell.Container{Name: name, Image: cell.Image{Repository: repository, Digest: digest}}
		for _, raw := range sliceField(container, "volumeMounts") {
			mount, _ := raw.(map[string]any)
			volume, _ := mount["name"].(string)
			if audience, carries := tokens[volume]; carries {
				mounts[name] = append(mounts[name], tokenMount{volume: volume, audience: audience})
			}
		}
		containers = append(containers, rendered)
	}
	return containers, nil
}

// overlayManifests reads the manifests of a unit's overlay: the kustomize build
// when the overlay has a kustomization, the YAML files themselves otherwise. A
// unit an agent rendered always has one; a tree assembled directly in a test
// may not, and what it says about its workloads is the same either way.
func overlayManifests(overlay string) ([]manifest, error) {
	for _, name := range []string{kustomizationFile, kustomizationFileAlt} {
		if _, err := os.Stat(filepath.Join(overlay, name)); err == nil {
			kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
			built, buildErr := kustomizer.Run(filesys.MakeFsOnDisk(), overlay)
			if buildErr != nil {
				return nil, fmt.Errorf("build %s: %w", overlay, buildErr)
			}
			output, encodeErr := built.AsYaml()
			if encodeErr != nil {
				return nil, fmt.Errorf("encode %s: %w", overlay, encodeErr)
			}
			manifests, _, decodeErr := decodeYAML("kustomize:"+filepath.ToSlash(overlay), output)
			return manifests, decodeErr
		}
	}
	entries, err := os.ReadDir(overlay)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", overlay, err)
	}
	var manifests []manifest
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), yamlExtension) && !strings.HasSuffix(entry.Name(), ymlExtension)) {
			continue
		}
		data, readErr := readWithin(overlay, entry.Name())
		if readErr != nil {
			return nil, readErr
		}
		decoded, _, decodeErr := decodeYAML(filepath.ToSlash(filepath.Join(overlay, entry.Name())), data)
		if decodeErr != nil {
			return nil, decodeErr
		}
		manifests = append(manifests, decoded...)
	}
	return manifests, nil
}

// podSelector is the exact label set that selects a workload's pods: the
// selector a Deployment, StatefulSet or DaemonSet declares, or the pod
// template's own labels for a Job or CronJob, which carry no selector of their
// own. Always non-nil: an empty selector is a declaration that the workload's
// pods carry no label, which a policy must see rather than assume "app".
// podSelector is the exact label set that selects a workload's pods. The cell
// file carries a label set and nothing else, so a selector written with
// matchExpressions is refused rather than reduced: an expression-only
// selector would come out as {}, which a loader reads as "every pod of the
// namespace" or as "no pod", and either is a security policy derived from a
// constraint that was never carried.
func podSelector(item manifest) (map[string]string, error) {
	selector := map[string]string{}
	var labels map[string]any
	switch item.kind {
	case kindDeployment, kindStatefulSet, kindDaemonSet:
		spec := mapField(mapField(item.value, "spec"), "selector")
		if expressions := sliceField(spec, "matchExpressions"); len(expressions) > 0 {
			return nil, fmt.Errorf("workload %s selects its pods with matchExpressions, which the cell file cannot carry as the exact label set a policy selects by; select the pods by labels alone", metadataString(item.value, "name"))
		}
		labels = mapField(spec, "matchLabels")
	default:
		labels = mapField(mapField(podTemplate(item), "metadata"), "labels")
	}
	for key, value := range labels {
		selector[key] = fmt.Sprint(value)
	}
	return selector, nil
}

// endpointConsumers indexes, for every endpoint a composed service declares,
// the module-qualified services that declare a dependency on it: a
// service-dependencies entry naming endpoints reaches those, one naming none
// reaches every endpoint of the service it names. A dependency the workspace
// cannot resolve to a service is not an edge and is skipped.
func endpointConsumers(ctx context.Context, workspace *resources.Workspace) (map[string][]string, error) {
	consumers := map[string][]string{}
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
					key := resources.ServiceUnique(targetModule, target.Name) + "/" + name
					if !slices.Contains(consumers[key], consumer) {
						consumers[key] = append(consumers[key], consumer)
					}
				}
			}
		}
	}
	for key := range consumers {
		sort.Strings(consumers[key])
	}
	return consumers, nil
}
