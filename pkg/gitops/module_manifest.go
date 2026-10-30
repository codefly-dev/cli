package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

// --- The module manifest ---
//
// A module (or a composed solution) is present on a host because a signed
// module manifest declared it, never because its process announced itself.
// core's solutionhost package owns the document — ModuleManifest, its rules,
// its canonical bytes and its signature — and this file builds one per module
// instance: which workloads run as the module (every service declaring
// module-identity), where the host may route to (its routed endpoints, with
// their addresses), and what the module asks for (its contract, as its
// authority request). A cell's render and a native run build it the same way
// (ModuleManifestOf); they differ only in where a workload's account and an
// endpoint's address come from.
//
// In a cell the document travels in a ConfigMap of the module's owned tree:
// the promotable ruleset refuses any YAML there that is not a Kubernetes
// manifest or a Kustomization. Publish settles its generation, signs it and
// writes the Job that POSTs it to the host.

const (
	// moduleManifestDir is the render subdirectory holding the module's
	// manifest. It carries the "<dir>/overlays/<environment>" shape every
	// delivered path does, because an Argo Application is pointed at an
	// overlay.
	moduleManifestDir = "module-manifests"

	// documentLabel marks a ConfigMap carrying a delivery document, and
	// moduleManifestKind is its value for a manifest; bindingLabel carries the
	// binding the document declares.
	documentLabel      = "codefly.dev/document"
	moduleManifestKind = "module-manifest"
	// pairedDeliveryKind is its value for a PAIRED delivery document: the
	// module manifest's signed carrier and the authority declaration signed
	// against it, in the one request the composition interface takes. It is a
	// separate kind because a reader of a delivered overlay must tell the two
	// apart — a paired document is not a manifest and does not parse as one.
	pairedDeliveryKind = "paired-delivery"
	bindingLabel       = "codefly.dev/binding"
	managedByLabel     = "app.kubernetes.io/managed-by"
	managedByCodefly   = "codefly"
	// kindConfigMap is the carrier's kind, named once so the manifest it
	// writes and the tests that read it back cannot drift apart.
	kindConfigMap = "ConfigMap"
)

// moduleManifestOverlay is the delivered overlay of the manifest directory for
// one environment.
func moduleManifestOverlay(environment string) string {
	return filepath.ToSlash(filepath.Join(moduleManifestDir, "overlays", environment))
}

// rfc1123Label is one lowercase DNS label: what a route alias and every part
// of a binding ID must be, narrower than core's patterns, because the binding
// names a Kubernetes object and the alias is one URL path segment.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// bindingIDMaxLength bounds a binding ID to one Kubernetes label value, which
// is stricter than core's 128: the ID is stamped as a label.
const bindingIDMaxLength = 63

// ModuleInstance is one module instance of the resolved composition, as the
// caller resolved it: everything a manifest says about it except the accounts
// its workloads run as, which a cell reads off its rendered pods and a native
// run off its own identities.
type ModuleInstance struct {
	// Kind is whether the instance is a composed solution or a module.
	Kind solutionhost.Kind
	// Name is the instance's name in the composition: its binding's last
	// part, its route alias and its principal.
	Name string
	// Package and Version are the module package this instance resolved to:
	// "<publisher>/<name>" and an exact semantic version.
	Package string
	Version string
	// Frontend is the component's optional frontend block, with its
	// generated payload.
	Frontend *solutionhost.Frontend
	// Group is the presentation group a solution is organised under; nil for
	// a module and for a solution in no group.
	Group *solutionhost.SolutionGroup
	// Identities are the services that run as the module: every one that
	// declares module-identity, with the unit that renders it in a cell.
	Identities []IdentityWorkload
	// Endpoints are the module's endpoints the host routes, each with the
	// address the composition resolved for it.
	Endpoints []solutionhost.Endpoint
	// Authority is what the module asks for. Its principal is always the
	// module's name.
	Authority *solutionhost.Authority
}

// IdentityWorkload is one service that runs as its module's principal, and
// the tree-relative directory of the unit that renders it (empty natively).
type IdentityWorkload struct {
	Service string
	Path    string
}

// ModuleManifestOf is the manifest of one instance at generation 1, its
// workloads as the caller attested them. Its generation is settled separately,
// against what was delivered before.
func ModuleManifestOf(binding string, host *environments.EnvironmentHost, instance *ModuleInstance, workloads []solutionhost.Workload) (*solutionhost.ModuleManifest, error) {
	if host == nil {
		return nil, fmt.Errorf("%s %s: the environment declares no host", instance.Kind, instance.Name)
	}
	publisher, name, err := releaseIdentity(instance)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", instance.Kind, instance.Name, err)
	}
	if !rfc1123Label.MatchString(instance.Name) {
		return nil, fmt.Errorf("%s %s claims the route alias %q; a host keys its registry on the alias as one URL path segment, so it must be lowercase letters, digits and dashes, starting and ending alphanumeric",
			instance.Kind, instance.Name, instance.Name)
	}
	manifest := &solutionhost.ModuleManifest{
		Schema:     solutionhost.SchemaModuleManifestV1,
		Binding:    binding,
		Alias:      instance.Name,
		Kind:       instance.Kind,
		Generation: 1,
		Host:       solutionhost.HostTarget{Coordinate: host.Coordinate, Domain: host.Domain},
		Release:    solutionhost.Release{Publisher: publisher, Name: name, Version: instance.Version},
		Group:      instance.Group,
		Frontend:   instance.Frontend,
		Workloads:  workloads,
		Endpoints:  instance.Endpoints,
		Authority:  instance.Authority,
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("%s %s declares an invalid module manifest: %w", instance.Kind, instance.Name, err)
	}
	return manifest, nil
}

// renderModuleManifest renders the instance's manifest into the staged tree,
// with the account the delivery Job runs as and the overlay's kustomization.
//
// owned is the staged tree; destination is the tree this render replaces,
// where the previously rendered generation is read from. It runs before
// validateTree so the ConfigMap is held to the promotable ruleset, and before
// buildInventory so it is hashed into the render digest.
func renderModuleManifest(owned, destination string, opts *RenderOptions) (*DeclaredModuleManifest, error) {
	tree, rootErr := inspectRenderRoot(owned, opts)
	if rootErr != nil {
		return nil, rootErr
	}
	defer releaseRenderRoot(tree, opts)

	instance := opts.Instance
	if instance == nil {
		return nil, nil
	}
	if opts.Host == nil {
		// A derived host would be a coordinate a host silently refuses at
		// reconcile time; an environment that composes a module declares the
		// host it runs on.
		return nil, fmt.Errorf("environment %s composes %s but declares no host block (coordinate, component, domain, audience, trust_domain, delivery), so nothing declares it present anywhere; declare the host the environment runs on", opts.Environment, instance.Name)
	}
	if err := opts.Host.Validate(); err != nil {
		return nil, fmt.Errorf("environment %s host: %w", opts.Environment, err)
	}
	binding, err := bindingID(opts.Workspace, opts.Environment, instance.Name)
	if err != nil {
		return nil, err
	}
	workloads := make([]solutionhost.Workload, 0, len(instance.Identities))
	for _, identity := range instance.Identities {
		workload, workloadErr := renderedIdentityWorkload(tree, owned, opts, identity)
		if workloadErr != nil {
			return nil, fmt.Errorf("%s %s: %w", instance.Kind, instance.Name, workloadErr)
		}
		workloads = append(workloads, workload)
	}
	manifest, err := ModuleManifestOf(binding, opts.Host, instance, workloads)
	if err != nil {
		return nil, err
	}
	// Provisional: publish settles the generation against the delivery
	// repository's base branch. The render keeps the generation of the tree
	// it replaces when nothing changed, so a re-render is byte-identical.
	if manifest.Generation, err = nextGeneration(destination, opts.Environment, manifest); err != nil {
		return nil, err
	}
	directory := filepath.Join(owned, filepath.FromSlash(moduleManifestOverlay(opts.Environment)))
	if err = tree.mkdirAll(directory); err != nil {
		return nil, fmt.Errorf("create module manifest directory: %w", err)
	}
	file, err := writeModuleManifestInTree(tree, directory, opts.Namespace, manifest, nil)
	if err != nil {
		return nil, err
	}
	// The account the delivery Job runs as is rendered here; the Job itself
	// is written at publish, once the manifest is settled and signed.
	account, err := renderDeliveryServiceAccountInTree(tree, directory, opts.Namespace)
	if err != nil {
		return nil, err
	}
	if err = writeModuleManifestKustomizationInTree(tree, owned, opts.Environment, []string{file, account}); err != nil {
		return nil, err
	}
	return &DeclaredModuleManifest{
		Path: filepath.ToSlash(filepath.Join(moduleManifestOverlay(opts.Environment), file)), Binding: binding, Generation: manifest.Generation,
	}, nil
}

// renderedIdentityWorkload reads one identity service's workload off the unit
// that renders it: the container that authenticates and the account it runs
// as, which names its SPIFFE ID. Every serving workload of the unit (its
// Deployments, StatefulSets, DaemonSets) must agree on both; a bootstrap Job
// never authenticates as the service and is not read.
func renderedIdentityWorkload(tree *renderRoot, owned string, opts *RenderOptions, identity IdentityWorkload) (solutionhost.Workload, error) {
	all, err := renderedWorkloadsInTree(tree, filepath.Join(owned, filepath.FromSlash(identity.Path)), opts.Environment)
	if err != nil {
		return solutionhost.Workload{}, fmt.Errorf("service %s: %w", identity.Service, err)
	}
	var found *solutionhost.Workload
	for index := range all {
		workload := &all[index]
		if !servingKind(workload.Kind) {
			continue
		}
		switch workload.ServiceAccount {
		case deliveryServiceAccount:
			// The delivery account is the delivery Job's: a workload running
			// as it would be a deployment caller and a runtime caller at once.
			return solutionhost.Workload{}, fmt.Errorf("service %s workload %s runs as the %q ServiceAccount, which is reserved for the delivery Job; a workload that runs as its module names an account of its own", identity.Service, workload.Name, deliveryServiceAccount)
		case defaultServiceAccount:
			return solutionhost.Workload{}, fmt.Errorf("service %s workload %s runs as the namespace's default ServiceAccount, whose identity every pod of the namespace that names no account shares; a workload that runs as its module names its own account (spec.template.spec.serviceAccountName)", identity.Service, workload.Name)
		}
		container, err := authenticatingContainer(identity.Service, workload)
		if err != nil {
			return solutionhost.Workload{}, fmt.Errorf("service %s workload %s: %w", identity.Service, workload.Name, err)
		}
		// A manifest workload names no account: the cell spells it by rule,
		// the ServiceAccount named after the service. A render that spells it
		// differently is refused here, not discovered at the first mint. The
		// namespace is not checked here: it is the module's delivered
		// namespace, which the render takes from the one function stating
		// that rule (environments.Environment.ModuleNamespace).
		if workload.ServiceAccount != workloadAccount(identity.Service) {
			return solutionhost.Workload{}, fmt.Errorf("service %s workload %s runs as ServiceAccount %q; a module's service runs as the account named after it (%q), which is how the host knows the pod",
				identity.Service, workload.Name, workload.ServiceAccount, workloadAccount(identity.Service))
		}
		candidate := solutionhost.Workload{Service: identity.Service, Container: container.Name}
		if found != nil && *found != candidate {
			return solutionhost.Workload{}, fmt.Errorf("service %s renders serving workloads that authenticate as %s (%s) and %s (%s); a service runs as one account from one container", identity.Service, found.Service, found.Container, candidate.Service, candidate.Container)
		}
		found = &candidate
	}
	if found == nil {
		return solutionhost.Workload{}, fmt.Errorf("service %s declares module-identity and renders no serving workload (a Deployment, StatefulSet or DaemonSet) to run as the module", identity.Service)
	}
	return *found, nil
}

// bindingID is the stable ID of one deployment instance: the workspace that
// composed it, the environment it is delivered to, and its instance name. It
// is derived from identity alone — no digest, no generation, no address — so
// it never changes between renders, and each part is one lowercase DNS label
// because the ID names a Kubernetes object.
func bindingID(workspace, environment, instance string) (string, error) {
	for _, part := range []struct{ label, value string }{
		{"workspace", workspace}, {"environment", environment}, {"instance", instance},
	} {
		if part.value == "" {
			return "", fmt.Errorf("module manifest binding ID needs the %s name", part.label)
		}
		if !rfc1123Label.MatchString(part.value) {
			return "", fmt.Errorf(
				"module manifest binding ID cannot be built from %s %q: the ID names a Kubernetes object, so each part must be lowercase letters, digits and dashes, starting and ending alphanumeric",
				part.label, part.value)
		}
	}
	id := strings.Join([]string{workspace, environment, instance}, ".")
	if len(id) > bindingIDMaxLength {
		return "", fmt.Errorf("module manifest binding ID %q is %d characters; it is stamped as a Kubernetes label value, so it must be at most %d", id, len(id), bindingIDMaxLength)
	}
	return id, nil
}

// releaseIdentity splits a module package ID ("<publisher>/<name>") into the
// two halves a release names, refusing anything else rather than guessing.
func releaseIdentity(instance *ModuleInstance) (string, string, error) {
	publisher, name, found := strings.Cut(instance.Package, "/")
	if !found || publisher == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("module package %q is not <publisher>/<name>, so the release it declares cannot be named", instance.Package)
	}
	if instance.Version == "" {
		return "", "", fmt.Errorf("module package %s resolved to no version, so the release it declares cannot be pinned", instance.Package)
	}
	return publisher, name, nil
}

// nextGeneration settles a candidate's generation against the one the tree
// being replaced holds for the same binding: unchanged content keeps it,
// changed content is the next one, and no prior manifest is generation 1.
// "Unchanged" is core's canonical digest, the encoding the host tells a replay
// from a rewrite by. A render that cannot see the replaced tree emits 1, and
// publish — which reads the base branch — settles the real number.
func nextGeneration(destination, environment string, candidate *solutionhost.ModuleManifest) (uint64, error) {
	prior, err := priorRenderedManifest(destination, environment, candidate.Binding)
	if err != nil || prior == nil {
		return 1, err
	}
	return settledGeneration(prior, candidate)
}

// priorRenderedManifest reads the manifest the tree being replaced rendered for
// this binding, or nil when it rendered none. An unreadable one is an error,
// never a silent generation 1.
func priorRenderedManifest(destination, environment, binding string) (*solutionhost.ModuleManifest, error) {
	data, err := readWithin(destination, filepath.Join(filepath.FromSlash(moduleManifestOverlay(environment)), binding+".yaml"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the rendered module manifest %q: %w", binding, err)
	}
	manifest, _, held, err := manifestFromConfigMap(data)
	if err != nil {
		return nil, fmt.Errorf("the rendered module manifest %q: %w", binding, err)
	}
	if !held || manifest.Binding != binding {
		return nil, fmt.Errorf("%s does not carry the module manifest of binding %q", filepath.ToSlash(filepath.Join(moduleManifestOverlay(environment), binding+".yaml")), binding)
	}
	return manifest, nil
}

// moduleManifestConfigMap carries one manifest, written through a struct so
// the delivered YAML has a stable field order and a re-render with no change
// produces identical bytes.
type moduleManifestConfigMap struct {
	APIVersion string                      `yaml:"apiVersion"`
	Kind       string                      `yaml:"kind"`
	Metadata   moduleManifestConfigMapMeta `yaml:"metadata"`
	Data       map[string]string           `yaml:"data"`
}

type moduleManifestConfigMapMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

// moduleManifestConfigMapName is the carrier's object name for a binding.
func moduleManifestConfigMapName(binding string) string {
	return moduleManifestKind + "-" + binding
}

// writeModuleManifestInTree writes one manifest's ConfigMap into directory —
// the readable document under core's FileName and, once signed, the carrier
// under its SignedFileName — and returns the file name.
func writeModuleManifestInTree(tree *renderRoot, directory, namespace string, manifest *solutionhost.ModuleManifest, carrier []byte) (string, error) {
	encoded, err := solutionhost.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal module manifest %q: %w", manifest.Binding, err)
	}
	configMap := moduleManifestConfigMap{
		APIVersion: "v1",
		Kind:       kindConfigMap,
		Metadata: moduleManifestConfigMapMeta{
			Name:      moduleManifestConfigMapName(manifest.Binding),
			Namespace: namespace,
			Labels: map[string]string{
				managedByLabel: managedByCodefly,
				documentLabel:  moduleManifestKind,
				bindingLabel:   manifest.Binding,
			},
		},
		Data: map[string]string{solutionhost.FileName: string(encoded)},
	}
	if len(carrier) > 0 {
		configMap.Data[solutionhost.SignedFileName] = string(carrier)
	}
	body, err := yaml.Marshal(configMap)
	if err != nil {
		return "", fmt.Errorf("encode module manifest %q: %w", manifest.Binding, err)
	}
	file := manifest.Binding + ".yaml"
	if err := tree.write(directory, file, body); err != nil {
		return "", fmt.Errorf("write module manifest %q: %w", manifest.Binding, err)
	}
	return file, nil
}

// manifestFromConfigMap reads the manifest a ConfigMap carries, with its signed
// carrier when it has one, and reports false for any other manifest in the
// overlay (the Job, the account). A ConfigMap labelled a manifest carrier that
// lost its document is a corrupt carrier, not another manifest.
func manifestFromConfigMap(data []byte) (*solutionhost.ModuleManifest, []byte, bool, error) {
	var carrier moduleManifestConfigMap
	if err := yaml.Unmarshal(data, &carrier); err != nil {
		return nil, nil, false, fmt.Errorf("decode: %w", err)
	}
	if carrier.Kind != kindConfigMap {
		return nil, nil, false, nil
	}
	encoded, held := carrier.Data[solutionhost.FileName]
	if !held {
		if carrier.Metadata.Labels[documentLabel] == moduleManifestKind {
			return nil, nil, false, fmt.Errorf("the ConfigMap %s is labelled a module manifest carrier and holds no %s", carrier.Metadata.Name, solutionhost.FileName)
		}
		return nil, nil, false, nil
	}
	manifest, err := solutionhost.Parse([]byte(encoded))
	if err != nil {
		return nil, nil, false, fmt.Errorf("the document is not a module manifest this Core reads: %w", err)
	}
	var signed []byte
	if raw, present := carrier.Data[solutionhost.SignedFileName]; present {
		signed = []byte(raw)
	}
	return manifest, signed, true, nil
}

// writeModuleManifestKustomization lists the overlay's manifests so it
// builds: Argo applies what the kustomization names.
func writeModuleManifestKustomization(owned, environment string, names []string) error {
	tree, err := newRenderRoot(owned)
	if err != nil {
		return err
	}
	defer tree.Close()
	return writeModuleManifestKustomizationInTree(tree, owned, environment, names)
}

func writeModuleManifestKustomizationInTree(tree *renderRoot, owned, environment string, names []string) error {
	sort.Strings(names)
	body, err := yaml.Marshal(kustomizationManifest{APIVersion: kustomizeAPIVersion, Kind: kindKustomization, Resources: names})
	if err != nil {
		return fmt.Errorf("encode module manifest kustomization: %w", err)
	}
	path := filepath.Join(owned, filepath.FromSlash(moduleManifestOverlay(environment)), kustomizationFile)
	if err = tree.write(filepath.Dir(path), filepath.Base(path), body); err != nil {
		return fmt.Errorf("write module manifest kustomization: %w", err)
	}
	return nil
}

// deliveredManifestPath is the render subdirectory the inventory records,
// derived from what this render wrote, unconditionally: a value left on a
// reused RenderOptions would claim a path the tree does not have.
func deliveredManifestPath(declared *DeclaredModuleManifest) string {
	if declared == nil {
		return ""
	}
	return moduleManifestDir
}

// solutionGroupOf is the group block a solution's manifest carries: the
// composing workspace's solution group the instance is listed in (core's
// Workspace.SolutionGroupOf), generated here and authored nowhere else. A
// module is grouped by nothing; a group grants and installs nothing.
func solutionGroupOf(workspace *resources.Workspace, instance *ModuleInstance) *solutionhost.SolutionGroup {
	if workspace == nil || instance == nil || instance.Kind != solutionhost.KindSolution {
		return nil
	}
	group, ok := workspace.SolutionGroupOf(instance.Name)
	if !ok {
		return nil
	}
	return &solutionhost.SolutionGroup{ID: group.ID, Title: group.Title, Order: group.Order}
}

// moduleInstanceOf resolves the instance a module declares: a composed
// solution when the module ships a solution manifest, the module otherwise.
// It returns the reason when the module declares none — no package manifest
// to name a release from, or no service that runs as the module — so the
// render says so rather than leave an absence that reads as a composition with
// nothing in it. carriers are the addresses the composition resolved for the module's routed
// endpoints, keyed as the gateway reads them.
func moduleInstanceOf(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	services []*resources.Service,
	env *environments.Environment,
	pkg *InventoryPackage,
	artifacts []composition.ReleaseArtifact,
	carriers map[string]string,
) (*ModuleInstance, string, error) {
	request, err := authorityRequestOf(module)
	if err != nil {
		return nil, "", err
	}
	if pkg == nil {
		if request.undeclared == "" {
			// A request with no manifest to carry it would be dropped rather
			// than refused.
			return nil, "", fmt.Errorf("module %s %s but has no package manifest, so it names no release and declares no module manifest to carry the request", module.Name, request.source)
		}
		return nil, fmt.Sprintf("module %s has no package manifest, so it names no release and declares no module manifest", module.Name), nil
	}
	instance := &ModuleInstance{Kind: solutionhost.KindModule, Name: module.Name, Package: pkg.ID, Version: pkg.Version}
	declared, err := solutionrun.ModuleSolution(module)
	if err != nil {
		return nil, "", fmt.Errorf("read solution manifest of module %s: %w", module.Name, err)
	}
	if declared != nil {
		instance.Kind = solutionhost.KindSolution
	}
	identities := map[string]bool{}
	for _, service := range services {
		if !service.ModuleIdentity {
			continue
		}
		identities[service.Name] = true
		instance.Identities = append(instance.Identities, IdentityWorkload{Service: service.Name})
	}
	if len(instance.Identities) == 0 {
		if request.undeclared == "" {
			// The request is the module's ask for authority; with nothing
			// running as the module it would be dropped rather than refused.
			return nil, "", fmt.Errorf("module %s %s, which asks for authority, but no service declares module-identity, so nothing would run as it; declare module-identity on every service that runs as the module, or withdraw the request", module.Name, request.source)
		}
		return nil, fmt.Sprintf("module %s declares no module-identity service, so nothing runs as it and it declares no module manifest", module.Name), nil
	}
	if instance.Frontend, err = solutionrun.DeclaredFrontend(ctx, module); err != nil {
		return nil, "", err
	}
	if err = stampFrontendPayload(instance, artifacts); err != nil {
		return nil, "", err
	}
	if instance.Endpoints, err = routedEndpoints(module.Name, services, carriers, identities); err != nil {
		return nil, "", err
	}
	if instance.Authority, err = moduleAuthority(ctx, workspace, module, services, env, request); err != nil {
		return nil, "", err
	}
	instance.Group = solutionGroupOf(workspace, instance)
	return instance, "", nil
}

// bindIdentityUnits names, for each identity workload of a cell render, the
// unit that renders it.
func bindIdentityUnits(instance *ModuleInstance, units []InventoryUnit) error {
	for index := range instance.Identities {
		identity := &instance.Identities[index]
		for position := range units {
			if units[position].Name == identity.Service && units[position].Path != "" && !units[position].Managed {
				identity.Path = units[position].Path
			}
		}
		if identity.Path == "" {
			return fmt.Errorf("module %s service %s declares module-identity but this render delivers no unit for it, so nothing runs as the module there", instance.Name, identity.Service)
		}
	}
	return nil
}

// routedEndpoints is the module's endpoints the host routes — the ones the
// composition resolved a carrier for — each with that address. A host routes
// only to a workload that runs as the module, so a routed endpoint of any
// other service is refused by name. running, when given, narrows the set to
// the identity services that run now (a native run lists the ones it started).
func routedEndpoints(module string, services []*resources.Service, carriers map[string]string, identities map[string]bool, running ...map[string]bool) ([]solutionhost.Endpoint, error) {
	var endpoints []solutionhost.Endpoint
	for _, service := range services {
		for _, endpoint := range service.Endpoints {
			if endpoint == nil {
				continue
			}
			key := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: module, Service: service.Name, Name: endpoint.Name, API: endpoint.API})
			address, routed := carriers[key]
			if !routed {
				continue
			}
			if !identities[service.Name] {
				return nil, fmt.Errorf("module %s service %s endpoint %s is routed by the host, and a host routes only to a workload that runs as the module; declare module-identity on %s", module, service.Name, endpoint.Name, service.Name)
			}
			if len(running) > 0 && !running[0][service.Name] {
				continue
			}
			endpoints = append(endpoints, solutionhost.Endpoint{Name: endpoint.Name, Service: service.Name, API: endpoint.API, Address: address})
		}
	}
	return endpoints, nil
}
