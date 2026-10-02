package gitops

import (
	"context"
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
	// CellSchemaV1 is the cell file's schema.
	CellSchemaV1 = "codefly/cell/v1"
	// CellFileName is the cell file's name within its environment directory.
	CellFileName = "cell.yaml"
	cellsDir     = "cells"
)

// CellFile is the inventory of one environment's cell.
type CellFile struct {
	Schema      string `yaml:"schema"`
	Coordinate  string `yaml:"coordinate,omitempty"`
	Component   string `yaml:"component,omitempty"`
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
}

// CellWorkload is one thing the host runs: a Deployment or StatefulSet of a
// rendered unit, with the identity it runs as and the endpoints it serves.
type CellWorkload struct {
	// Name is the workload's object name.
	Name string `yaml:"name"`
	// Service is the module-qualified service the workload runs: <module>/<service>.
	Service string `yaml:"service"`
	// ServiceAccount is the account the pod runs as.
	ServiceAccount string `yaml:"service_account"`
	// SPIFFEID is the identity the host's issuer gives that account, when the
	// environment declares a host trust domain.
	SPIFFEID string `yaml:"spiffe_id,omitempty"`
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
}

// CellContainer is one container and its pinned image.
type CellContainer struct {
	Name  string    `yaml:"name"`
	Image CellImage `yaml:"image"`
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
	Name         string   `yaml:"name"`
	API          string   `yaml:"api,omitempty"`
	Port         uint32   `yaml:"port,omitempty"`
	Visibility   string   `yaml:"visibility,omitempty"`
	AllowModules []string `yaml:"allow_modules,omitempty"`
}

// CellIngress is one ingress route into an endpoint.
type CellIngress struct {
	Endpoint string   `yaml:"endpoint"`
	Hosts    []string `yaml:"hosts"`
}

// CellEgress is the external reach a managed service grants.
type CellEgress struct {
	Service string   `yaml:"service"`
	CIDRs   []string `yaml:"cidrs"`
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
		cell.Coordinate, cell.Component = env.Host.Coordinate, env.Host.Component
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
		namespace, namespaceErr := cellNamespace(ctx, workspace, env, tree, &inventory)
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
	if err := os.WriteFile(result.Path, body, 0o644); err != nil { //nolint:gosec // an inventory of public manifest identities, readable beside the rendered trees
		return CellResult{}, fmt.Errorf("write cell file: %w", err)
	}
	return result, nil
}

// cellNamespace derives one module's namespace entry from its rendered tree.
func cellNamespace(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, tree string, inventory *Inventory) (CellNamespace, error) {
	namespace := CellNamespace{Name: inventory.Namespace, Module: inventory.Module}
	var release *CellRelease
	if inventory.Package != nil {
		publisher, name, found := strings.Cut(inventory.Package.ID, "/")
		if found {
			release = &CellRelease{Publisher: publisher, Name: name, Version: inventory.Package.Version}
		}
	}
	for _, unit := range inventory.Units {
		if unit.Kind != UnitKindService || unit.Path == "" || unit.Managed {
			continue
		}
		service, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: unit.Name, Module: inventory.Module})
		if err != nil {
			return CellNamespace{}, fmt.Errorf("load service %s/%s of the rendered tree: %w", inventory.Module, unit.Name, err)
		}
		ports, err := declaredEndpointPorts(service)
		if err != nil {
			return CellNamespace{}, err
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
		for index := range workloads {
			workload := &workloads[index]
			workload.Service = resources.ServiceUnique(inventory.Module, unit.Name)
			workload.Artifact = CellArtifact{Name: unit.Name, Digest: digest}
			workload.Release = release
			if env.Host != nil {
				workload.SPIFFEID = env.Host.SPIFFEID(inventory.Namespace, workload.ServiceAccount)
			}
			for _, endpoint := range service.Endpoints {
				if endpoint == nil {
					continue
				}
				workload.Endpoints = append(workload.Endpoints, CellEndpoint{
					Name: endpoint.Name, API: endpoint.API, Port: ports[endpoint.Name],
					Visibility: endpoint.Visibility, AllowModules: append([]string(nil), endpoint.AllowModules...),
				})
			}
			sort.Slice(workload.Endpoints, func(i, j int) bool { return workload.Endpoints[i].Name < workload.Endpoints[j].Name })
			for _, route := range env.Ingress {
				if route.Service == unit.Name || route.Service == workload.Service {
					workload.Ingress = append(workload.Ingress, CellIngress{Endpoint: route.Endpoint, Hosts: append([]string(nil), route.Hosts...)})
				}
			}
			sort.Slice(workload.Ingress, func(i, j int) bool { return workload.Ingress[i].Endpoint < workload.Ingress[j].Endpoint })
			namespace.Workloads = append(namespace.Workloads, *workload)
		}
	}
	sort.Slice(namespace.Workloads, func(i, j int) bool { return namespace.Workloads[i].Name < namespace.Workloads[j].Name })
	for _, unit := range inventory.Units {
		if unit.Kind != UnitKindService {
			continue
		}
		managed, declared := env.ManagedService(inventory.Module, unit.Name)
		if !declared || len(managed.EgressCIDRs) == 0 {
			continue
		}
		namespace.Egress = append(namespace.Egress, CellEgress{
			Service: resources.ServiceUnique(inventory.Module, unit.Name), CIDRs: append([]string(nil), managed.EgressCIDRs...),
		})
	}
	sort.Slice(namespace.Egress, func(i, j int) bool { return namespace.Egress[i].Service < namespace.Egress[j].Service })
	return namespace, nil
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
		case kindDeployment, kindStatefulSet, kindDaemonSet:
		default:
			continue
		}
		spec, ok := podSpec(item)
		if !ok {
			continue
		}
		workload := CellWorkload{Name: metadataString(item.value, "name")}
		workload.ServiceAccount, _ = spec["serviceAccountName"].(string)
		if workload.ServiceAccount == "" {
			workload.ServiceAccount = "default"
		}
		containers, err := renderedContainers(workload.Name, sliceField(spec, "containers"))
		if err != nil {
			return nil, err
		}
		workload.Containers = containers
		if workload.InitContainers, err = renderedContainers(workload.Name, sliceField(spec, "initContainers")); err != nil {
			return nil, err
		}
		workloads = append(workloads, workload)
	}
	return workloads, nil
}

// renderedContainers reads a pod template's containers and their pinned images.
func renderedContainers(workload string, raw []any) ([]CellContainer, error) {
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
		containers = append(containers, CellContainer{Name: name, Image: CellImage{Repository: repository, Digest: digest}})
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
		data, readErr := os.ReadFile(filepath.Join(overlay, entry.Name())) //nolint:gosec // a file of the overlay being read
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
