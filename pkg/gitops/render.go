package gitops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

var (
	digestImagePattern = regexp.MustCompile(`^.+@sha256:[a-fA-F0-9]{64}$`)
	digestPattern      = regexp.MustCompile(`^sha256:[a-fA-F0-9]{64}$`)
	placeholderPattern = regexp.MustCompile(`(?i)(\$\{[^}]+\}|\{\{[^}]+\}\}|<<[^>]+>>|\bCHANGE_?ME\b|\bREPLACE_?ME\b)`)
)

const argoAPIGroup = "argoproj.io"

var clusterScopedKinds = map[string]struct{}{
	"APIService": {}, "CSIDriver": {}, "CSINode": {}, "ClusterIssuer": {},
	"ClusterRole": {}, "ClusterRoleBinding": {}, "CustomResourceDefinition": {},
	"IngressClass": {}, "MutatingWebhookConfiguration": {}, "Namespace": {},
	"Node": {}, "PersistentVolume": {}, "PriorityClass": {}, "RuntimeClass": {},
	"StorageClass": {}, "ValidatingWebhookConfiguration": {}, "VolumeAttachment": {},
}

type manifest struct {
	path  string
	group string
	kind  string
	value map[string]any
}

type kustomization struct {
	path       string
	directory  string
	references []string
}

type projectContract struct {
	name             string
	destinations     map[string]struct{}
	clusterResources map[string]struct{}
}

func RenderOwnedTree(ctx context.Context, opts *RenderOptions, generate func(context.Context, string) error) (RenderResult, error) {
	if opts.Destination == "" {
		return RenderResult{}, fmt.Errorf("render destination is required")
	}
	destination, err := filepath.Abs(opts.Destination)
	if err != nil {
		return RenderResult{}, fmt.Errorf("resolve render destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return RenderResult{}, fmt.Errorf("create render parent: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".codefly-render-")
	if err != nil {
		return RenderResult{}, fmt.Errorf("create render staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	owned := filepath.Join(stage, "tree")
	if err := os.Mkdir(owned, 0o755); err != nil {
		return RenderResult{}, fmt.Errorf("create staged owned tree: %w", err)
	}
	if err := generate(ctx, owned); err != nil {
		return RenderResult{}, fmt.Errorf("generate staged manifests: %w", err)
	}
	if err := validateTree(owned, opts); err != nil {
		return RenderResult{}, err
	}
	inventory, err := buildInventory(owned, opts)
	if err != nil {
		return RenderResult{}, err
	}
	canonical, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return RenderResult{}, fmt.Errorf("encode render inventory: %w", err)
	}
	canonical = append(canonical, '\n')
	// The inventory contains public manifest identities and must remain inspectable beside the rendered files.
	if err := os.WriteFile(filepath.Join(owned, InventoryFilename), canonical, 0o644); err != nil { //nolint:gosec
		return RenderResult{}, fmt.Errorf("write render inventory: %w", err)
	}
	if err := replaceOwnedTree(owned, destination); err != nil {
		return RenderResult{}, err
	}
	return RenderResult{Path: destination, Inventory: inventory}, nil
}

func LoadInventory(root string) (Inventory, error) {
	return loadInventory(filepath.Join(root, InventoryFilename), "render")
}

func loadInventory(path, label string) (Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Inventory{}, fmt.Errorf("read %s inventory: %w", label, err)
	}
	var inventory Inventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return Inventory{}, fmt.Errorf("decode %s inventory: %w", label, err)
	}
	if inventory.SchemaVersion != SchemaVersion {
		return Inventory{}, fmt.Errorf("unsupported %s inventory schema %d", label, inventory.SchemaVersion)
	}
	canonical, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return Inventory{}, fmt.Errorf("encode %s inventory: %w", label, err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return Inventory{}, fmt.Errorf("%s inventory is not canonical", label)
	}
	return inventory, nil
}

func ValidateRenderedTree(root, project string, promotable bool) error {
	inventory, err := LoadInventory(root)
	if err != nil {
		return err
	}
	if project == "" {
		project = inventory.AppProject
	} else if inventory.AppProject != project {
		return fmt.Errorf("render inventory AppProject %q differs from selected AppProject %q", inventory.AppProject, project)
	}
	if err := validateInventoryServiceGraph(&inventory); err != nil {
		return err
	}
	opts := &RenderOptions{
		Module: inventory.Module, Service: inventory.Service,
		OwnedPath: inventory.OwnedPath, ServiceGraph: inventory.ServiceGraph,
		Environment: inventory.Environment, AppProject: project, Promotable: promotable,
	}
	for _, service := range inventory.ServiceGraph {
		if !service.Managed {
			opts.Services = append(opts.Services, service.Service)
		}
	}
	if err := validateTree(root, opts); err != nil {
		return err
	}
	actual, err := buildInventory(root, opts)
	if err != nil {
		return err
	}
	return validateInventory(&inventory, &actual, "render")
}

func ValidateServiceSnapshot(root string) error {
	inventory, err := LoadInventory(root)
	if err != nil {
		return err
	}
	if err := validateInventoryServiceGraph(&inventory); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != InventoryFilename && entry.Name() != "services" {
			return fmt.Errorf("service snapshot contains unexpected path %s", entry.Name())
		}
	}
	services := filepath.Join(root, "services")
	var names []string
	for _, service := range inventory.ServiceGraph {
		if !service.Managed {
			names = append(names, service.Service)
		}
	}
	if err := validateServiceDirectories(root, names, inventory.Environment); err != nil {
		return err
	}
	for _, service := range names {
		opts := &RenderOptions{
			Module: inventory.Module, Service: service,
			Environment: inventory.Environment, AppProject: inventory.AppProject, Promotable: true,
		}
		if err := validateTree(filepath.Join(services, service), opts); err != nil {
			return fmt.Errorf("validate service %s: %w", service, err)
		}
	}
	if err := validateServiceSnapshotCoverage(&inventory); err != nil {
		return err
	}
	opts := &RenderOptions{
		Module: inventory.Module, Services: names,
		OwnedPath: inventory.OwnedPath, ServiceGraph: inventory.ServiceGraph,
		Environment: inventory.Environment, AppProject: inventory.AppProject, Promotable: true,
	}
	actual, err := buildInventory(root, opts)
	if err != nil {
		return err
	}
	return validateInventory(&inventory, &actual, "service snapshot")
}

func validateServiceSnapshotCoverage(inventory *Inventory) error {
	covered := make(map[string]bool)
	for _, service := range inventory.ServiceGraph {
		if !service.Managed {
			covered[service.Path] = false
		}
	}
	for _, file := range inventory.Files {
		owner := ""
		for servicePath := range covered {
			if file.Path == servicePath || strings.HasPrefix(file.Path, servicePath+"/") {
				if owner != "" {
					return fmt.Errorf("service snapshot file %s belongs to overlapping service paths", file.Path)
				}
				owner = servicePath
			}
		}
		if owner == "" {
			return fmt.Errorf("service snapshot file %s is outside the exact service graph", file.Path)
		}
		covered[owner] = true
	}
	for servicePath, present := range covered {
		if !present {
			return fmt.Errorf("service snapshot path %s contains no files", servicePath)
		}
	}
	return nil
}

func validateInventoryServiceGraph(inventory *Inventory) error {
	if inventory.Service != "" {
		if len(inventory.ServiceGraph) != 0 {
			return fmt.Errorf("service render inventory must not contain a module service graph")
		}
		return nil
	}
	previous := ""
	for _, service := range inventory.ServiceGraph {
		if service.Service == "" || service.Module != inventory.Module {
			return fmt.Errorf("render inventory contains invalid service graph entry %q/%q", service.Module, service.Service)
		}
		if previous != "" && service.Service <= previous {
			return fmt.Errorf("render inventory service graph is not strictly sorted")
		}
		previous = service.Service
		if service.Managed {
			if service.Path != "" || service.Output != nil {
				return fmt.Errorf("managed service %s must not contain a rendered path or output", service.Service)
			}
			continue
		}
		expectedPath := filepath.ToSlash(filepath.Join("services", service.Service))
		if service.Path != expectedPath {
			return fmt.Errorf("service %s render path is %q, expected %q", service.Service, service.Path, expectedPath)
		}
		if inventory.OwnedPath == "" {
			continue
		}
		if err := validateInventoryKubernetesOutput(service.Service, service.Output); err != nil {
			return err
		}
	}
	return nil
}

func validateInventoryKubernetesOutput(service string, output *KubernetesOutputInventory) error {
	if output == nil {
		return fmt.Errorf("service %s has no promotable Kubernetes output evidence", service)
	}
	if output.Kind != builderv0.KubernetesDeploymentOutput_KUSTOMIZE.String() ||
		output.Profile != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1.String() ||
		output.ContractVersion != coreservices.KubernetesManifestContractVersion {
		return fmt.Errorf("service %s has incompatible Kubernetes output evidence", service)
	}
	passed := builderv0.KubernetesManifestValidation_STATUS_PASSED.String()
	if output.Validation.StaticValidation != passed ||
		output.Validation.ServerSideValidation != passed ||
		!output.Validation.Promotable ||
		output.Validation.Violations == nil ||
		len(output.Validation.Violations) != 0 {
		return fmt.Errorf("service %s has failed promotable Kubernetes validation evidence", service)
	}
	return nil
}

func validateInventory(inventory, actual *Inventory, label string) error {
	if actual.Digest != inventory.Digest {
		return fmt.Errorf("%s digest changed: inventory has %s, tree has %s", label, inventory.Digest, actual.Digest)
	}
	if len(actual.Files) != len(inventory.Files) {
		return fmt.Errorf("%s inventory changed: inventory has %d files, tree has %d", label, len(inventory.Files), len(actual.Files))
	}
	for i := range actual.Files {
		if actual.Files[i] != inventory.Files[i] {
			return fmt.Errorf("%s inventory changed at %s", label, actual.Files[i].Path)
		}
	}
	return nil
}

func validateTree(root string, opts *RenderOptions) error {
	if opts.Service == "" && len(opts.Services) > 0 {
		if err := validateServiceDirectories(root, opts.Services, opts.Environment); err != nil {
			return err
		}
	}
	var manifests []manifest
	var kustomizations []kustomization
	err := walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		if relative == InventoryFilename {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", relative, err)
		}
		if !utf8.Valid(data) {
			return fmt.Errorf("%s is not UTF-8", relative)
		}
		if placeholderPattern.Match(data) {
			return fmt.Errorf("%s contains an unresolved placeholder", relative)
		}
		extension := strings.ToLower(filepath.Ext(relative))
		if extension != ".yaml" && extension != ".yml" && extension != ".json" {
			return nil
		}
		decoded, customization, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		manifests = append(manifests, decoded...)
		if customization != nil {
			kustomizations = append(kustomizations, *customization)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(manifests) == 0 && len(kustomizations) == 0 {
		return fmt.Errorf("rendered tree contains no Kubernetes manifests")
	}
	contract, err := selectProjectContract(manifests, opts.AppProject)
	if err != nil {
		return err
	}
	for _, item := range manifests {
		if err := validateManifest(item, contract, false); err != nil {
			return fmt.Errorf("%s: %w", item.path, err)
		}
	}
	if !opts.Promotable {
		return nil
	}
	covered, effective, err := renderKustomizations(root, kustomizations)
	if err != nil {
		return err
	}
	for _, item := range effective {
		if err := validateManifest(item, contract, true); err != nil {
			return fmt.Errorf("%s: %w", item.path, err)
		}
	}
	for _, item := range manifests {
		source := strings.SplitN(item.path, "#", 2)[0]
		if covered[source] {
			continue
		}
		if err := validateManifest(item, contract, true); err != nil {
			return fmt.Errorf("%s: %w", item.path, err)
		}
	}
	return nil
}

func validateServiceDirectories(root string, services []string, environment string) error {
	serviceRoot := filepath.Join(root, "services")
	entries, err := os.ReadDir(serviceRoot)
	if err != nil {
		return fmt.Errorf("read rendered service graph: %w", err)
	}
	expected := make(map[string]struct{}, len(services))
	for _, service := range services {
		expected[service] = struct{}{}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("rendered service graph contains unexpected file %s", entry.Name())
		}
		if _, exists := expected[entry.Name()]; !exists {
			return fmt.Errorf("rendered service graph contains unexpected service %s", entry.Name())
		}
		delete(expected, entry.Name())
	}
	if len(expected) > 0 {
		missing := make([]string, 0, len(expected))
		for service := range expected {
			missing = append(missing, service)
		}
		sort.Strings(missing)
		return fmt.Errorf("rendered service graph is missing services %v", missing)
	}
	for _, service := range services {
		overlay := filepath.Join(serviceRoot, service, "overlays", environment)
		info, err := os.Stat(overlay)
		if err != nil {
			return fmt.Errorf("service %s environment overlay %s: %w", service, environment, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("service %s environment overlay %s is not a directory", service, environment)
		}
	}
	return nil
}

func decodeYAML(path string, data []byte) ([]manifest, *kustomization, error) {
	var manifests []manifest
	var customization *kustomization
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for document := 1; ; document++ {
		var value any
		err := decoder.Decode(&value)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s document %d: decode YAML: %w", path, document, err)
		}
		if value == nil {
			continue
		}
		root, ok := value.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s document %d: YAML root must be a mapping", path, document)
		}
		if strings.EqualFold(filepath.Ext(path), ".json") && root["apiVersion"] == nil && root["kind"] == nil {
			continue
		}
		base := strings.ToLower(filepath.Base(path))
		if base == "kustomization.yaml" || base == "kustomization.yml" || base == "kustomization" || root["kind"] == "Kustomization" {
			if customization != nil {
				return nil, nil, fmt.Errorf("%s contains multiple Kustomization documents", path)
			}
			found, err := validateKustomization(path, root)
			if err != nil {
				return nil, nil, err
			}
			customization = &kustomization{
				path: path, directory: filepath.ToSlash(filepath.Dir(filepath.FromSlash(path))),
				references: found,
			}
			continue
		}
		decoded, err := decodeManifest(path, fmt.Sprintf("%d", document), root)
		if err != nil {
			return nil, nil, err
		}
		manifests = append(manifests, decoded...)
	}
	return manifests, customization, nil
}

func decodeManifest(path, location string, root map[string]any) ([]manifest, error) {
	apiVersion, _ := root["apiVersion"].(string)
	kind, _ := root["kind"].(string)
	if apiVersion == "" || kind == "" {
		return nil, fmt.Errorf("%s document %s: Kubernetes manifest requires apiVersion and kind", path, location)
	}
	if apiVersion == "v1" && kind == "List" {
		items, ok := root["items"].([]any)
		if !ok {
			return nil, fmt.Errorf("%s document %s: Kubernetes List items must be an array", path, location)
		}
		var manifests []manifest
		for index, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s document %s item %d: Kubernetes manifest must be a mapping", path, location, index)
			}
			decoded, err := decodeManifest(path, fmt.Sprintf("%s.items[%d]", location, index), item)
			if err != nil {
				return nil, err
			}
			manifests = append(manifests, decoded...)
		}
		return manifests, nil
	}
	group := apiVersion
	if slash := strings.IndexByte(group, '/'); slash >= 0 {
		group = group[:slash]
	} else {
		group = ""
	}
	return []manifest{{path: fmt.Sprintf("%s#%s", path, location), group: group, kind: kind, value: root}}, nil
}

func validateKustomization(path string, root map[string]any) ([]string, error) {
	if generators, ok := root["secretGenerator"].([]any); ok && len(generators) > 0 {
		return nil, fmt.Errorf("%s: kustomize secretGenerator values are not allowed", path)
	}
	if err := inspectValue(root, nil, false); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var references []string
	for _, key := range []string{"resources", "bases", "components", "patchesStrategicMerge"} {
		values, _ := root[key].([]any)
		for _, raw := range values {
			value, ok := raw.(string)
			if !ok {
				continue
			}
			if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
				return nil, fmt.Errorf("%s: remote kustomize %s %q is not allowed", path, key, value)
			}
			clean := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(path)), filepath.FromSlash(value)))
			if filepath.IsAbs(filepath.FromSlash(value)) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s: kustomize %s %q escapes the owned tree", path, key, value)
			}
			if key == "resources" || key == "bases" || key == "components" {
				references = append(references, filepath.ToSlash(clean))
			}
		}
	}
	images, _ := root["images"].([]any)
	for _, raw := range images {
		image, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := image["name"].(string)
		digest, _ := image["digest"].(string)
		if digest == "" {
			continue
		}
		if !digestPattern.MatchString(digest) {
			return nil, fmt.Errorf("%s: kustomize image %q has invalid digest %q", path, name, digest)
		}
	}
	return references, nil
}

func renderKustomizations(root string, kustomizations []kustomization) (map[string]bool, []manifest, error) {
	covered := map[string]bool{}
	if len(kustomizations) == 0 {
		return covered, nil, nil
	}
	byDirectory := map[string]kustomization{}
	referencedDirectories := map[string]bool{}
	for _, customization := range kustomizations {
		byDirectory[customization.directory] = customization
		for _, reference := range customization.references {
			if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(reference))); err == nil && info.IsDir() {
				referencedDirectories[reference] = true
			}
		}
	}
	var roots []kustomization
	for _, customization := range kustomizations {
		if !referencedDirectories[customization.directory] {
			roots = append(roots, customization)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].path < roots[j].path })
	var effective []manifest
	for _, customization := range roots {
		markKustomizationCoverage(root, customization, byDirectory, covered, map[string]bool{})
		kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
		resources, err := kustomizer.Run(filesys.MakeFsOnDisk(), filepath.Join(root, filepath.FromSlash(customization.directory)))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: build Kustomize output: %w", customization.path, err)
		}
		output, err := resources.AsYaml()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: encode Kustomize output: %w", customization.path, err)
		}
		renderedPath := "kustomize:" + filepath.ToSlash(filepath.Join(customization.directory, "rendered.yaml"))
		decoded, nested, err := decodeYAML(renderedPath, output)
		if err != nil {
			return nil, nil, err
		}
		if nested != nil {
			return nil, nil, fmt.Errorf("%s: Kustomize output contains a Kustomization", customization.path)
		}
		effective = append(effective, decoded...)
	}
	return covered, effective, nil
}

func markKustomizationCoverage(root string, customization kustomization, byDirectory map[string]kustomization, covered, visiting map[string]bool) {
	if visiting[customization.directory] {
		return
	}
	visiting[customization.directory] = true
	for _, reference := range customization.references {
		path := filepath.Join(root, filepath.FromSlash(reference))
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			if nested, ok := byDirectory[reference]; ok {
				markKustomizationCoverage(root, nested, byDirectory, covered, visiting)
			}
			continue
		}
		covered[reference] = true
	}
	delete(visiting, customization.directory)
}

func selectProjectContract(manifests []manifest, selected string) (*projectContract, error) {
	projects := map[string]*projectContract{}
	for _, item := range manifests {
		if item.group != argoAPIGroup || item.kind != "AppProject" {
			continue
		}
		name := metadataString(item.value, "name")
		if name == "" {
			return nil, fmt.Errorf("%s: AppProject metadata.name is required", item.path)
		}
		contract := &projectContract{name: name, destinations: map[string]struct{}{}, clusterResources: map[string]struct{}{}}
		spec, _ := item.value["spec"].(map[string]any)
		destinations, _ := spec["destinations"].([]any)
		for _, raw := range destinations {
			destination, _ := raw.(map[string]any)
			namespace, _ := destination["namespace"].(string)
			server, _ := destination["server"].(string)
			name, _ := destination["name"].(string)
			if strings.Contains(namespace, "*") || strings.Contains(server, "*") || strings.Contains(name, "*") {
				return nil, fmt.Errorf("%s: AppProject %s contains wildcard authority", item.path, contract.name)
			}
			if namespace != "" {
				contract.destinations[namespace] = struct{}{}
			}
		}
		whitelist, _ := spec["clusterResourceWhitelist"].([]any)
		for _, raw := range whitelist {
			resource, _ := raw.(map[string]any)
			group, _ := resource["group"].(string)
			kind, _ := resource["kind"].(string)
			if strings.Contains(group, "*") || strings.Contains(kind, "*") {
				return nil, fmt.Errorf("%s: AppProject %s contains wildcard cluster authority", item.path, contract.name)
			}
			if kind != "" {
				contract.clusterResources[group+"/"+kind] = struct{}{}
			}
		}
		projects[name] = contract
	}
	if selected != "" {
		contract, ok := projects[selected]
		if !ok {
			if len(projects) == 0 {
				return &projectContract{
					name: selected, destinations: map[string]struct{}{}, clusterResources: map[string]struct{}{},
				}, nil
			}
			return nil, fmt.Errorf("selected AppProject %q is not present in rendered manifests", selected)
		}
		return contract, nil
	}
	if len(projects) == 1 {
		for _, contract := range projects {
			return contract, nil
		}
	}
	if len(projects) > 1 {
		return nil, fmt.Errorf("multiple AppProjects rendered; select one explicitly")
	}
	return nil, nil
}

func validateManifest(item manifest, contract *projectContract, promotable bool) error {
	if item.kind == "Secret" {
		if promotable {
			return fmt.Errorf("secret resources are not allowed")
		}
		for _, key := range []string{"data", "stringData"} {
			if values, ok := item.value[key].(map[string]any); ok && len(values) > 0 {
				return fmt.Errorf("Kubernetes Secret values are not allowed")
			}
		}
	}
	_, knownClusterScoped := clusterScopedKinds[item.kind]
	customClusterScoped := item.group != "" && !isBuiltInAPIGroup(item.group) &&
		item.group != argoAPIGroup && metadataString(item.value, "namespace") == ""
	if knownClusterScoped || customClusterScoped {
		if contract == nil {
			return fmt.Errorf("cluster-scoped %s is outside an AppProject contract", item.kind)
		}
		if _, allowed := contract.clusterResources[item.group+"/"+item.kind]; !allowed {
			return fmt.Errorf("cluster-scoped %s is not declared by AppProject %s", item.kind, contract.name)
		}
		if item.kind == "Namespace" {
			name := metadataString(item.value, "name")
			if _, allowed := contract.destinations[name]; !allowed {
				return fmt.Errorf("namespace %s is outside AppProject %s destinations", name, contract.name)
			}
		}
	}
	if item.group == argoAPIGroup && item.kind == "Application" && contract != nil {
		spec, _ := item.value["spec"].(map[string]any)
		project, _ := spec["project"].(string)
		if project != contract.name {
			return fmt.Errorf("Application project %q differs from selected AppProject %q", project, contract.name)
		}
	}
	return inspectValue(item.value, nil, promotable)
}

func isBuiltInAPIGroup(group string) bool {
	switch group {
	case "apps", "autoscaling", "batch", "coordination.k8s.io", "discovery.k8s.io",
		"events.k8s.io", "extensions", "networking.k8s.io", "policy",
		"rbac.authorization.k8s.io", "scheduling.k8s.io", "storage.k8s.io":
		return true
	default:
		return false
	}
}

func inspectValue(value any, path []string, promotable bool) error {
	switch typed := value.(type) {
	case map[string]any:
		if name, ok := typed["name"].(string); ok && isCredentialKey(strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(name))) && scalarHasValue(typed["value"]) {
			return fmt.Errorf("%s.value contains credential value", strings.Join(path, "."))
		}
		for key, child := range typed {
			next := extendPath(path, key)
			normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(key))
			if isCredentialKey(normalized) && scalarHasValue(child) {
				return fmt.Errorf("%s contains credential value", strings.Join(next, "."))
			}
			if key == "image" && promotable {
				image, ok := child.(string)
				if ok && !digestImagePattern.MatchString(image) {
					return fmt.Errorf("%s image %q is not digest-pinned", strings.Join(next, "."), image)
				}
			}
			if err := inspectValue(child, next, promotable); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := inspectValue(child, extendPath(path, fmt.Sprintf("[%d]", index)), promotable); err != nil {
				return err
			}
		}
	case string:
		if placeholderPattern.MatchString(typed) {
			return fmt.Errorf("%s contains an unresolved placeholder", strings.Join(path, "."))
		}
		if isURLBearingPath(path) {
			if err := validateURLValue(strings.Join(path, "."), typed); err != nil {
				return err
			}
		}
		if isAuthorityPath(path) && strings.Contains(typed, "*") {
			return fmt.Errorf("%s contains wildcard authority", strings.Join(path, "."))
		}
	}
	return nil
}

func isURLBearingPath(path []string) bool {
	for index := len(path) - 1; index >= 0; index-- {
		part := path[index]
		if strings.HasPrefix(part, "[") {
			continue
		}
		normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(part))
		return normalized == "server" ||
			normalized == "sourcerepos" ||
			strings.HasSuffix(normalized, "url") ||
			strings.HasSuffix(normalized, "uri")
	}
	return false
}

func extendPath(path []string, part string) []string {
	extended := make([]string, len(path)+1)
	copy(extended, path)
	extended[len(path)] = part
	return extended
}

func validateURLValue(path, value string) error {
	if digestPattern.MatchString(value) {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return nil
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "grpcs", "ssh":
	default:
		return fmt.Errorf("%s contains unsafe URL scheme %q", path, parsed.Scheme)
	}
	if parsed.User != nil {
		return fmt.Errorf("%s URL contains credentials", path)
	}
	if strings.Contains(parsed.Hostname(), "*") {
		return fmt.Errorf("%s URL contains wildcard authority", path)
	}
	return nil
}

func isCredentialKey(normalized string) bool {
	for _, fragment := range []string{"password", "passwd", "token", "credential", "privatekey", "clientsecret", "accesskey", "secretkey"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func scalarHasValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []byte:
		return len(typed) > 0
	default:
		return false
	}
}

func isAuthorityPath(path []string) bool {
	for _, part := range path {
		switch strings.ToLower(part) {
		case "sourcerepos", "sourcenamespaces", "destination", "destinations",
			"clusterresourcewhitelist", "namespaceresourcewhitelist",
			"apigroups", "resources", "verbs", "nonresourceurls":
			return true
		}
	}
	if len(path) == 0 {
		return false
	}
	key := strings.ToLower(path[len(path)-1])
	return key == "host" || key == "hostname" || key == "server" || key == "address" || key == "url" || key == "repourl"
}

func metadataString(value map[string]any, key string) string {
	metadata, _ := value["metadata"].(map[string]any)
	result, _ := metadata[key].(string)
	return result
}

func buildInventory(root string, opts *RenderOptions) (Inventory, error) {
	inventory := Inventory{
		SchemaVersion: SchemaVersion,
		Module:        opts.Module, Service: opts.Service, Environment: opts.Environment,
		AppProject: opts.AppProject, OwnedPath: filepath.ToSlash(opts.OwnedPath),
		ServiceGraph: append([]InventoryService(nil), opts.ServiceGraph...),
	}
	if len(inventory.ServiceGraph) == 0 {
		for _, service := range opts.Services {
			inventory.ServiceGraph = append(inventory.ServiceGraph, InventoryService{
				Module: opts.Module, Service: service, Path: filepath.ToSlash(filepath.Join("services", service)),
			})
		}
	}
	sort.Slice(inventory.ServiceGraph, func(i, j int) bool {
		return inventory.ServiceGraph[i].Service < inventory.ServiceGraph[j].Service
	})
	hash := sha256.New()
	err := walkRegularFiles(root, func(path, relative string, info os.FileInfo) error {
		if relative == InventoryFilename {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", relative, err)
		}
		defer file.Close()
		fileHash := sha256.New()
		if _, err := io.Copy(fileHash, file); err != nil {
			return fmt.Errorf("hash %s: %w", relative, err)
		}
		digest := hex.EncodeToString(fileHash.Sum(nil))
		inventory.Files = append(inventory.Files, InventoryFile{Path: filepath.ToSlash(relative), SHA256: "sha256:" + digest, Size: info.Size()})
		return nil
	})
	if err != nil {
		return Inventory{}, err
	}
	sort.Slice(inventory.Files, func(i, j int) bool { return inventory.Files[i].Path < inventory.Files[j].Path })
	for _, file := range inventory.Files {
		fmt.Fprintf(hash, "%s\x00%s\x00%d\n", file.Path, file.SHA256, file.Size)
	}
	inventory.Digest = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return inventory, nil
}

func walkRegularFiles(root string, visit func(path, relative string, info os.FileInfo) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symbolic links are not allowed in rendered output", relative)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: non-regular files are not allowed in rendered output", relative)
		}
		return visit(path, relative, info)
	})
}

func replaceOwnedTree(stage, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		if err := exchangeDirectories(stage, destination); err != nil {
			return fmt.Errorf("atomically replace rendered owned tree: %w", err)
		}
		if err := os.RemoveAll(stage); err != nil {
			return fmt.Errorf("remove previous owned tree: %w", err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect previous owned tree: %w", err)
	}
	if err := os.Rename(stage, destination); err != nil {
		return fmt.Errorf("install rendered owned tree: %w", err)
	}
	return nil
}

func copyTree(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symbolic links are not allowed", relative)
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: non-regular files are not allowed", relative)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			input.Close()
			return err
		}
		writer := bufio.NewWriter(output)
		_, copyErr := io.Copy(writer, input)
		inputErr := input.Close()
		flushErr := writer.Flush()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputErr != nil {
			return inputErr
		}
		if flushErr != nil {
			return flushErr
		}
		return closeErr
	})
}
