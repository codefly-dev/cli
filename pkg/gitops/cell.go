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
	// CellSchemaV1 is the cell file's schema.
	CellSchemaV1 = "codefly/cell/v1"
	// CellFileName is the cell file's name within its environment directory.
	CellFileName = "cell.yaml"
	cellsDir     = "cells"
)

// CellFile is the inventory of one environment's cell.
type CellFile struct {
	Schema     string `yaml:"schema"`
	Coordinate string `yaml:"coordinate,omitempty"`
	Component  string `yaml:"component,omitempty"`
	// Domain is the ownership domain this composition delivers under — the one
	// every document it renders asserts. A host accepts a domain only from a
	// signer it lets speak for it (core's Host.DomainsBySigner), and that
	// policy is the platform's, keyed by the composition's release-workflow
	// identity; carrying the domain here lets the platform hold its policy
	// against what the composition declares at build time, rather than have
	// the host refuse the first delivery.
	Domain string `yaml:"domain,omitempty"`
	// TrustDomain is the SPIFFE trust domain every workload's identity is
	// issued under, carried at the top level as well as inside each spiffe_id
	// so the platform can re-derive an identity and refuse a mismatch rather
	// than parse the domain back out of the string it is checking.
	TrustDomain string `yaml:"trust_domain,omitempty"`
	Environment string `yaml:"environment"`
	// Namespaces are the namespaces the composition's modules render into, one
	// per module, in name order.
	Namespaces []CellNamespace `yaml:"namespaces"`
}

// CellNamespace is one module's namespace and what runs in it.
type CellNamespace struct {
	Name      string         `yaml:"name"`
	Module    string         `yaml:"module"`
	Workloads []CellWorkload `yaml:"workloads"`
	// Egress is the external reach the environment grants workloads of this
	// namespace: the CIDRs of the managed services that replace its services.
	// It is the only egress the render holds.
	Egress []CellEgress `yaml:"egress,omitempty"`
	// Delivery is the Job that POSTs this namespace's presence documents to the
	// host, when the module delivers presence: a pod the closed admission set
	// would refuse unless declared, and the one pod of the namespace that runs
	// as the delivery account. Its name carries the settled set's digest and
	// is decided at publish, so it is declared by its labels.
	Delivery *CellDelivery `yaml:"delivery,omitempty"`
}

// CellDelivery is the presence delivery Job as admission must know it: the
// labels its pods carry, the account they run as, and the one container and
// image that deliver.
type CellDelivery struct {
	Kind           string            `yaml:"kind"`
	Selector       map[string]string `yaml:"selector"`
	ServiceAccount string            `yaml:"service_account"`
	SPIFFEID       string            `yaml:"spiffe_id,omitempty"`
	Container      string            `yaml:"container"`
	Image          CellImage         `yaml:"image"`
}

// CellWorkload is one thing the host runs — every pod-producing object of a
// rendered unit: a Deployment, StatefulSet or DaemonSet, and the Jobs and
// CronJobs that bootstrap it, which run their own images and must be declared
// or the platform's closed approved set refuses them — with the identity it
// runs as and the endpoints it serves.
type CellWorkload struct {
	// Name is the workload's object name.
	Name string `yaml:"name"`
	// Kind is the workload's Kubernetes kind.
	Kind string `yaml:"kind"`
	// Selector is the exact label set that selects the workload's pods: the
	// selector of a Deployment, StatefulSet or DaemonSet, the pod template's
	// labels of a Job or CronJob. Carried explicitly, because a bootstrap Job
	// carries no "app" label and a policy assuming one selects its pods with
	// nothing.
	Selector map[string]string `yaml:"selector"`
	// Service is the module-qualified service the workload runs: <module>/<service>.
	Service string `yaml:"service"`
	// ServiceAccount is the account the pod runs as.
	ServiceAccount string `yaml:"service_account"`
	// SPIFFEID is the identity the host's issuer gives that account, when the
	// environment declares a host trust domain.
	SPIFFEID string `yaml:"spiffe_id,omitempty"`
	// Authenticating names the one container that authenticates as the
	// workload — the same designation the presence document carries — so an
	// admission policy compares that container's image to the approved build
	// and treats every other container, init containers included, as a closed
	// set that never does. Inferring it from a single container is safe;
	// inferring it from several is the sidecar attack, so it is named.
	Authenticating string `yaml:"authenticating"`
	// Containers are the workload's containers and the exact image each runs.
	Containers []CellContainer `yaml:"containers"`
	// InitContainers are the workload's init containers, which never
	// authenticate as the workload.
	InitContainers []CellContainer `yaml:"init_containers,omitempty"`
	// Artifact is the rendered unit the workload comes from, pinned by the
	// digest of its rendered bytes.
	Artifact CellArtifact `yaml:"artifact"`
	// Release is the module package the unit was rendered from, when it has one.
	Release *CellRelease `yaml:"release,omitempty"`
	// Endpoints are the endpoints the service declares, with the port each is
	// served on when the service declares one.
	Endpoints []CellEndpoint `yaml:"endpoints,omitempty"`
	// Ingress are the environment's ingress routes to this workload's endpoints.
	Ingress []CellIngress `yaml:"ingress,omitempty"`
	// Verifier marks the serving workloads of the service the environment's
	// host block names as the delivery API: the one that verifies delivered
	// documents, from which the platform derives the narrow RBAC that needs
	// (token reviews, pod reads in delivered namespaces) and the carrier's
	// allow into it. Derived from host.delivery, never guessed.
	Verifier bool `yaml:"verifier,omitempty"`
	// Bindings are the cell-provided resources the service is declared to
	// bind, from the environment's cell declaration; the cell provisions each
	// and derives the grant.
	Bindings []string `yaml:"bindings,omitempty"`
	// CloudIdentity is whether the workload mints a cloud credential from the
	// node's metadata server, declared by the environment: a network path no
	// egress waypoint carries, which the platform allows per workload.
	CloudIdentity bool `yaml:"cloud_identity,omitempty"`
}

// CellContainer is one container and its pinned image.
type CellContainer struct {
	Name  string    `yaml:"name"`
	Image CellImage `yaml:"image"`
	// tokenMounts are the projected ServiceAccount token volumes minted for an
	// explicit audience that this container mounts. Not part of the cell file:
	// read so the render can refuse a token a sidecar could present.
	tokenMounts []tokenMount
}

// tokenMount is one projected ServiceAccount token volume carrying an
// explicit audience, as a container mounts it.
type tokenMount struct {
	volume   string
	audience string
}

// CellImage is an image reference split into the two things the platform
// compares: the repository, and the OCI manifest digest.
type CellImage struct {
	Repository string `yaml:"repository"`
	Digest     string `yaml:"digest"`
}

// CellArtifact is one rendered unit and the digest of its rendered bytes.
type CellArtifact struct {
	Name   string `yaml:"name"`
	Digest string `yaml:"digest"`
}

// CellRelease identifies the module package a unit was rendered from.
type CellRelease struct {
	Publisher string `yaml:"publisher"`
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`
}

// CellEndpoint is one declared endpoint of a workload's service.
type CellEndpoint struct {
	Name string `yaml:"name"`
	API  string `yaml:"api,omitempty"`
	// Port is the CONTAINER port the endpoint is served on — the port a
	// connection lands on after Service resolution, which is what a mesh
	// authorizes — as the service declares it under
	// spec.deployment.endpoint-ports and the render verifies against the
	// rendered Service's target.
	Port         uint32   `yaml:"port,omitempty"`
	Visibility   string   `yaml:"visibility,omitempty"`
	AllowModules []string `yaml:"allow_modules,omitempty"`
	// Consumers are the module-qualified services that declare a dependency
	// on this endpoint, from every composed service's service-dependencies.
	// Empty means no declared edge reaches it, which is visible here rather
	// than found by an audit.
	Consumers []string `yaml:"consumers,omitempty"`
}

// CellIngress is one ingress route into an endpoint.
type CellIngress struct {
	Endpoint string   `yaml:"endpoint"`
	Hosts    []string `yaml:"hosts"`
}

// CellEgress is the external reach one service is declared to need: the hosts
// the environment declares it dials (a declaration, never derived), each on
// the port it is reached on, and the CIDRs of a managed service that replaces
// it.
type CellEgress struct {
	Service string           `yaml:"service"`
	Hosts   []CellEgressHost `yaml:"hosts,omitempty"`
	CIDRs   []string         `yaml:"cidrs,omitempty"`
}

// CellEgressHost is one host and the port it is reached on. The port is always
// explicit here, because a mesh allows a (host, port) and a reader defaulting
// it would be a second place the default lives.
type CellEgressHost struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
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
	return filepath.Join(workspaceDir, "deployments", cellsDir, environment, CellFileName)
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
	cell := CellFile{Schema: CellSchemaV1, Environment: env.Name}
	if env.Host != nil {
		cell.Coordinate, cell.Component, cell.Domain, cell.TrustDomain = env.Host.Coordinate, env.Host.Component, env.Host.Domain, env.Host.TrustDomain
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
		namespace, namespaceErr := cellNamespace(ctx, workspace, env, tree, &inventory, consumers)
		if namespaceErr != nil {
			return CellResult{}, namespaceErr
		}
		cell.Namespaces = append(cell.Namespaces, namespace)
		result.Modules = append(result.Modules, inventory.Module)
	}
	sort.Slice(cell.Namespaces, func(i, j int) bool { return cell.Namespaces[i].Name < cell.Namespaces[j].Name })
	sort.Strings(result.Modules)
	sort.Strings(result.Skipped)
	body, err := yaml.Marshal(cell)
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
func cellNamespace(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, tree string, inventory *Inventory, consumers map[string][]string) (CellNamespace, error) {
	namespace := CellNamespace{Name: inventory.Namespace, Module: inventory.Module}
	var release *CellRelease
	if inventory.Package != nil {
		publisher, name, found := strings.Cut(inventory.Package.ID, "/")
		if found {
			release = &CellRelease{Publisher: publisher, Name: name, Version: inventory.Package.Version}
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
				return CellNamespace{}, fmt.Errorf("load service %s/%s of the rendered tree: %w", inventory.Module, unit.Name, err)
			}
			service = loaded
			if ports, err = declaredEndpointPorts(service); err != nil {
				return CellNamespace{}, err
			}
		}
		digest, err := unitDigest(tree, unit.Path)
		if err != nil {
			return CellNamespace{}, err
		}
		workloads, err := renderedWorkloads(filepath.Join(tree, filepath.FromSlash(unit.Path)), env.Name)
		if err != nil {
			return CellNamespace{}, fmt.Errorf("unit %s/%s: %w", inventory.Module, unit.Name, err)
		}
		if len(workloads) == 0 {
			return CellNamespace{}, fmt.Errorf("unit %s/%s renders no workload", inventory.Module, unit.Name)
		}
		grant, _ := env.CellWorkload(inventory.Module, unit.Name)
		for index := range workloads {
			workload := &workloads[index]
			workload.Service = resources.ServiceUnique(inventory.Module, unit.Name)
			workload.Artifact = CellArtifact{Name: unit.Name, Digest: digest}
			workload.Release = release
			if workload.ServiceAccount == deliveryServiceAccount {
				return CellNamespace{}, fmt.Errorf("unit %s/%s workload %s runs as the %q ServiceAccount, which is reserved for the delivery Job", inventory.Module, unit.Name, workload.Name, deliveryServiceAccount)
			}
			authenticating, _, err := authenticatingContainer(unit.Name, workload)
			if err != nil {
				return CellNamespace{}, fmt.Errorf("unit %s/%s workload %s: %w", inventory.Module, unit.Name, workload.Name, err)
			}
			workload.Authenticating = authenticating.Name
			workload.Bindings = append([]string(nil), grant.Bindings...)
			workload.CloudIdentity = grant.CloudIdentity
			if env.Host != nil {
				workload.SPIFFEID = env.Host.SPIFFEID(inventory.Namespace, workload.ServiceAccount)
				workload.Verifier = isDeliveryVerifier(env, workload)
			}
			// Endpoints, their consumers and ingress belong to the workloads
			// that serve them. A service's own bootstrap Job or CronJob runs
			// its image once and listens on nothing, so it declares none —
			// a policy derived from it would grant a Job the service's
			// reachability.
			if service == nil || !servesTraffic(workload.Kind) {
				namespace.Workloads = append(namespace.Workloads, *workload)
				continue
			}
			for _, endpoint := range service.Endpoints {
				if endpoint == nil {
					continue
				}
				workload.Endpoints = append(workload.Endpoints, CellEndpoint{
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
					workload.Ingress = append(workload.Ingress, CellIngress{Endpoint: route.Endpoint, Hosts: append([]string(nil), route.Hosts...)})
				}
			}
			sort.Slice(workload.Ingress, func(i, j int) bool { return workload.Ingress[i].Endpoint < workload.Ingress[j].Endpoint })
			namespace.Workloads = append(namespace.Workloads, *workload)
		}
	}
	sort.Slice(namespace.Workloads, func(i, j int) bool { return namespace.Workloads[i].Name < namespace.Workloads[j].Name })
	if env.Host != nil && inventory.SolutionHostBindingPath != "" {
		repository, digest, _ := strings.Cut(deliveryImage, "@")
		namespace.Delivery = &CellDelivery{
			Kind:           kindJob,
			Selector:       map[string]string{managedByLabel: managedByCodefly, deliveryLabel: deliveryPresence},
			ServiceAccount: deliveryServiceAccount,
			SPIFFEID:       env.Host.SPIFFEID(inventory.Namespace, deliveryServiceAccount),
			Container:      deliveryContainerName,
			Image:          CellImage{Repository: repository, Digest: digest},
		}
	}
	for _, unit := range inventory.Units {
		if unit.Kind != UnitKindService {
			continue
		}
		egress := CellEgress{Service: resources.ServiceUnique(inventory.Module, unit.Name)}
		for _, host := range env.EgressHosts(inventory.Module, unit.Name) {
			egress.Hosts = append(egress.Hosts, CellEgressHost{Name: host.Name, Port: host.Port})
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

// isDeliveryVerifier reports whether a workload is a serving workload of the
// service the environment's host block names as the delivery API. A bootstrap
// Job of that service runs its own image and verifies nothing, so only the
// kinds that serve are marked.
func isDeliveryVerifier(env *environments.Environment, workload *CellWorkload) bool {
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
func renderedWorkloads(unitDir, environment string) ([]CellWorkload, error) {
	overlay := filepath.Join(unitDir, "overlays", environment)
	manifests, err := overlayManifests(overlay)
	if err != nil {
		return nil, err
	}
	var workloads []CellWorkload
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
		workload := CellWorkload{Name: metadataString(item.value, "name"), Kind: item.kind, Selector: selector}
		workload.ServiceAccount, _ = spec["serviceAccountName"].(string)
		if workload.ServiceAccount == "" {
			workload.ServiceAccount = defaultServiceAccount
		}
		tokens := audienceTokenVolumes(sliceField(spec, "volumes"))
		containers, err := renderedContainers(workload.Name, sliceField(spec, "containers"), tokens)
		if err != nil {
			return nil, err
		}
		workload.Containers = containers
		if workload.InitContainers, err = renderedContainers(workload.Name, sliceField(spec, "initContainers"), tokens); err != nil {
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
func renderedContainers(workload string, raw []any, tokens map[string]string) ([]CellContainer, error) {
	var containers []CellContainer
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
		rendered := CellContainer{Name: name, Image: CellImage{Repository: repository, Digest: digest}}
		for _, raw := range sliceField(container, "volumeMounts") {
			mount, _ := raw.(map[string]any)
			volume, _ := mount["name"].(string)
			if audience, carries := tokens[volume]; carries {
				rendered.tokenMounts = append(rendered.tokenMounts, tokenMount{volume: volume, audience: audience})
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
