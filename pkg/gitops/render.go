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

	"gopkg.in/yaml.v3"
)

var (
	digestImagePattern = regexp.MustCompile(`^.+@sha256:[a-fA-F0-9]{64}$`)
	digestPattern      = regexp.MustCompile(`^sha256:[a-fA-F0-9]{64}$`)
	placeholderPattern = regexp.MustCompile(`(?i)(\$\{[^}]+\}|\{\{[^}]+\}\}|<<[^>]+>>|\bCHANGE_?ME\b|\bREPLACE_?ME\b)`)
)

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

type projectContract struct {
	name             string
	destinations     map[string]struct{}
	clusterResources map[string]struct{}
}

func RenderOwnedTree(ctx context.Context, opts RenderOptions, generate func(context.Context, string) error) (RenderResult, error) {
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
	if err := os.WriteFile(filepath.Join(owned, InventoryFilename), canonical, 0o644); err != nil {
		return RenderResult{}, fmt.Errorf("write render inventory: %w", err)
	}
	if err := replaceOwnedTree(owned, destination); err != nil {
		return RenderResult{}, err
	}
	return RenderResult{Path: destination, Inventory: inventory}, nil
}

func LoadInventory(root string) (Inventory, error) {
	data, err := os.ReadFile(filepath.Join(root, InventoryFilename))
	if err != nil {
		return Inventory{}, fmt.Errorf("read render inventory: %w", err)
	}
	var inventory Inventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return Inventory{}, fmt.Errorf("decode render inventory: %w", err)
	}
	if inventory.SchemaVersion != SchemaVersion {
		return Inventory{}, fmt.Errorf("unsupported render inventory schema %d", inventory.SchemaVersion)
	}
	canonical, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return Inventory{}, fmt.Errorf("encode render inventory: %w", err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return Inventory{}, fmt.Errorf("render inventory is not canonical")
	}
	return inventory, nil
}

func ValidateRenderedTree(root, project string, promotable bool) error {
	inventory, err := LoadInventory(root)
	if err != nil {
		return err
	}
	opts := RenderOptions{
		Module: inventory.Module, Service: inventory.Service,
		Environment: inventory.Environment, AppProject: project, Promotable: promotable,
	}
	if err := validateTree(root, opts); err != nil {
		return err
	}
	actual, err := buildInventory(root, opts)
	if err != nil {
		return err
	}
	if actual.Digest != inventory.Digest {
		return fmt.Errorf("render digest changed: inventory has %s, tree has %s", inventory.Digest, actual.Digest)
	}
	if len(actual.Files) != len(inventory.Files) {
		return fmt.Errorf("render inventory changed: inventory has %d files, tree has %d", len(inventory.Files), len(actual.Files))
	}
	for i := range actual.Files {
		if actual.Files[i] != inventory.Files[i] {
			return fmt.Errorf("render inventory changed at %s", actual.Files[i].Path)
		}
	}
	return nil
}

func validateTree(root string, opts RenderOptions) error {
	var manifests []manifest
	imageReplacements := map[string]struct{}{}
	err := walkRegularFiles(root, func(path, relative string, info os.FileInfo) error {
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
		if extension != ".yaml" && extension != ".yml" {
			return nil
		}
		decoded, replacements, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		manifests = append(manifests, decoded...)
		for name := range replacements {
			imageReplacements[name] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		return fmt.Errorf("rendered tree contains no Kubernetes manifests")
	}
	contract, err := selectProjectContract(manifests, opts.AppProject)
	if err != nil {
		return err
	}
	for _, item := range manifests {
		if err := validateManifest(item, contract, imageReplacements, opts.Promotable); err != nil {
			return fmt.Errorf("%s: %w", item.path, err)
		}
	}
	return nil
}

func decodeYAML(path string, data []byte) ([]manifest, map[string]struct{}, error) {
	var manifests []manifest
	replacements := map[string]struct{}{}
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
		if filepath.Base(path) == "kustomization.yaml" || root["kind"] == "Kustomization" {
			found, err := validateKustomization(path, root)
			if err != nil {
				return nil, nil, err
			}
			for name := range found {
				replacements[name] = struct{}{}
			}
			continue
		}
		apiVersion, _ := root["apiVersion"].(string)
		kind, _ := root["kind"].(string)
		if apiVersion == "" || kind == "" {
			return nil, nil, fmt.Errorf("%s document %d: Kubernetes manifest requires apiVersion and kind", path, document)
		}
		group := apiVersion
		if slash := strings.IndexByte(group, '/'); slash >= 0 {
			group = group[:slash]
		} else {
			group = ""
		}
		manifests = append(manifests, manifest{path: fmt.Sprintf("%s#%d", path, document), group: group, kind: kind, value: root})
	}
	return manifests, replacements, nil
}

func validateKustomization(path string, root map[string]any) (map[string]struct{}, error) {
	if generators, ok := root["secretGenerator"].([]any); ok && len(generators) > 0 {
		return nil, fmt.Errorf("%s: kustomize secretGenerator values are not allowed", path)
	}
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
		}
	}
	replacements := map[string]struct{}{}
	images, _ := root["images"].([]any)
	for _, raw := range images {
		image, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := image["name"].(string)
		newName, _ := image["newName"].(string)
		digest, _ := image["digest"].(string)
		if digest == "" {
			continue
		}
		if !digestPattern.MatchString(digest) {
			return nil, fmt.Errorf("%s: kustomize image %q has invalid digest %q", path, name, digest)
		}
		if name != "" {
			replacements[name] = struct{}{}
		}
		if newName != "" {
			replacements[newName] = struct{}{}
		}
	}
	return replacements, nil
}

func selectProjectContract(manifests []manifest, selected string) (*projectContract, error) {
	projects := map[string]*projectContract{}
	for _, item := range manifests {
		if item.group != "argoproj.io" || item.kind != "AppProject" {
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

func validateManifest(item manifest, contract *projectContract, imageReplacements map[string]struct{}, promotable bool) error {
	if item.kind == "Secret" {
		for _, key := range []string{"data", "stringData"} {
			if values, ok := item.value[key].(map[string]any); ok && len(values) > 0 {
				return fmt.Errorf("Kubernetes Secret values are not allowed")
			}
		}
	}
	_, knownClusterScoped := clusterScopedKinds[item.kind]
	customClusterScoped := item.group != "" && !isBuiltInAPIGroup(item.group) &&
		item.group != "argoproj.io" && metadataString(item.value, "namespace") == ""
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
	if item.group == "argoproj.io" && item.kind == "Application" && contract != nil {
		spec, _ := item.value["spec"].(map[string]any)
		project, _ := spec["project"].(string)
		if project != contract.name {
			return fmt.Errorf("Application project %q differs from selected AppProject %q", project, contract.name)
		}
	}
	return inspectValue(item.value, nil, imageReplacements, promotable)
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

func inspectValue(value any, path []string, imageReplacements map[string]struct{}, promotable bool) error {
	switch typed := value.(type) {
	case map[string]any:
		if name, ok := typed["name"].(string); ok && isCredentialKey(strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(name))) && scalarHasValue(typed["value"]) {
			return fmt.Errorf("%s.value contains credential value", strings.Join(path, "."))
		}
		for key, child := range typed {
			next := append(path, key)
			normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(key))
			if isCredentialKey(normalized) && scalarHasValue(child) {
				return fmt.Errorf("%s contains credential value", strings.Join(next, "."))
			}
			if key == "image" && promotable {
				image, ok := child.(string)
				if ok && !digestImagePattern.MatchString(image) {
					base := image
					if at := strings.IndexByte(base, '@'); at >= 0 {
						base = base[:at]
					}
					if colon := strings.LastIndexByte(base, ':'); colon > strings.LastIndexByte(base, '/') {
						base = base[:colon]
					}
					if _, replaced := imageReplacements[base]; !replaced {
						return fmt.Errorf("%s image %q is not digest-pinned", strings.Join(next, "."), image)
					}
				}
			}
			if err := inspectValue(child, next, imageReplacements, promotable); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := inspectValue(child, append(path, fmt.Sprintf("[%d]", index)), imageReplacements, promotable); err != nil {
				return err
			}
		}
	case string:
		if placeholderPattern.MatchString(typed) {
			return fmt.Errorf("%s contains an unresolved placeholder", strings.Join(path, "."))
		}
		if err := validateURLValue(strings.Join(path, "."), typed); err != nil {
			return err
		}
		if isAuthorityPath(path) && strings.Contains(typed, "*") {
			return fmt.Errorf("%s contains wildcard authority", strings.Join(path, "."))
		}
	}
	return nil
}

func validateURLValue(path, value string) error {
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

func buildInventory(root string, opts RenderOptions) (Inventory, error) {
	inventory := Inventory{
		SchemaVersion: SchemaVersion,
		Module:        opts.Module, Service: opts.Service, Environment: opts.Environment,
	}
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
	backup := destination + ".codefly-backup"
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf("render backup already exists at %s", backup)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect render backup: %w", err)
	}
	existed := false
	if _, err := os.Stat(destination); err == nil {
		existed = true
		if err := os.Rename(destination, backup); err != nil {
			return fmt.Errorf("move previous owned tree: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect previous owned tree: %w", err)
	}
	if err := os.Rename(stage, destination); err != nil {
		if existed {
			_ = os.Rename(backup, destination)
		}
		return fmt.Errorf("install rendered owned tree: %w", err)
	}
	if existed {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove previous owned tree backup: %w", err)
		}
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
