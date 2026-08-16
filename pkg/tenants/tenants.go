// Package tenants expands a tenant model into per-tenant Kustomize overlays.
//
// A module's deployment tree carries one shared base and, historically, one
// hand-authored overlay per environment. Multi-cloud tenancy multiplies that
// into N tenants × M clouds of near-identical directories where only the
// VirtualService host and the External Secrets store reference differ. This
// package generates those overlays/<tenant>-<cloud>/ directories from a single
// declarative model so the N×M matrix stops being hand-maintained.
package tenants

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// ModelSchema is the accepted schemaVersion for a tenant model file.
const ModelSchema = "codefly.dev/tenant-model/v1"

// generatedMarker is dropped into every overlay this package writes so
// regeneration can tell overlays it owns from hand-authored ones. Kustomize
// only reads kustomization.yaml and the resources it references, so this file
// is inert during a build.
const generatedMarker = ".codefly-tenant-overlay"

// The Kubernetes kinds whose per-tenant fields this package patches.
const (
	kindVirtualService = "VirtualService"
	kindExternalSecret = "ExternalSecret"
)

// segmentPattern constrains tenant and cloud identifiers to a single lowercase
// DNS label so <tenant>-<cloud> is a safe, collision-free path segment.
var segmentPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Model is the tenant matrix for one module's deployment tree.
type Model struct {
	SchemaVersion string `yaml:"schema-version"`
	// Base is the shared Kustomize base directory, relative to the tree root,
	// that every generated overlay layers on top of.
	Base    string   `yaml:"base"`
	Tenants []Tenant `yaml:"tenants"`
}

// Tenant is one deployment target. Per the tenant model only the ingress host
// and the secret store reference vary; everything else comes from the base.
type Tenant struct {
	Name  string `yaml:"name"`
	Cloud string `yaml:"cloud"`
	// Host is the VirtualService host this tenant is served on.
	Host string `yaml:"host"`
	// SecretStore is the External Secrets store name that resolves this
	// tenant's secrets. Optional: when empty no secret-store patch is emitted.
	SecretStore string `yaml:"secret-store"`
}

// Overlay is the directory name of a generated overlay.
func (t Tenant) Overlay() string {
	return t.Name + "-" + t.Cloud
}

// LoadModel reads and validates a tenant model file.
func LoadModel(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tenant model: %w", err)
	}
	var model Model
	if err := yaml.Unmarshal(data, &model); err != nil {
		return nil, fmt.Errorf("decode tenant model: %w", err)
	}
	if err := model.validate(); err != nil {
		return nil, err
	}
	return &model, nil
}

func (m *Model) validate() error {
	if m.SchemaVersion != ModelSchema {
		return fmt.Errorf("unsupported tenant model schema %q, expected %q", m.SchemaVersion, ModelSchema)
	}
	if m.Base == "" {
		return fmt.Errorf("tenant model requires a base directory")
	}
	if filepath.IsAbs(m.Base) || !filepath.IsLocal(m.Base) {
		return fmt.Errorf("tenant model base %q must be a path inside the deployment tree", m.Base)
	}
	if len(m.Tenants) == 0 {
		return fmt.Errorf("tenant model declares no tenants")
	}
	seen := make(map[string]struct{}, len(m.Tenants))
	for i := range m.Tenants {
		tenant := &m.Tenants[i]
		if !segmentPattern.MatchString(tenant.Name) {
			return fmt.Errorf("tenant name %q is not a valid lowercase DNS label", tenant.Name)
		}
		if !segmentPattern.MatchString(tenant.Cloud) {
			return fmt.Errorf("tenant %q cloud %q is not a valid lowercase DNS label", tenant.Name, tenant.Cloud)
		}
		if err := validateHost(tenant.Host); err != nil {
			return fmt.Errorf("tenant %q: %w", tenant.Overlay(), err)
		}
		overlay := tenant.Overlay()
		if _, exists := seen[overlay]; exists {
			return fmt.Errorf("tenant model repeats overlay %q", overlay)
		}
		seen[overlay] = struct{}{}
	}
	return nil
}

func validateHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("host is required")
	}
	if host != strings.TrimSpace(host) || strings.ContainsAny(host, " \t\n") {
		return fmt.Errorf("host %q contains whitespace", host)
	}
	if strings.Contains(host, "*") {
		return fmt.Errorf("host %q contains a wildcard", host)
	}
	return nil
}

// Result reports what a Generate run changed on disk.
type Result struct {
	// Written are the overlay directory names generated this run, sorted.
	Written []string
	// Removed are previously generated overlay directories that the current
	// model no longer declares and that were reconciled away, sorted.
	Removed []string
}

// Generate writes overlays/<tenant>-<cloud>/kustomization.yaml under root for
// every tenant in the model, layering the shared base and patching the
// VirtualService host and (when set) the External Secrets store reference.
//
// Each generated overlay is built through Kustomize and rejected unless the
// host patch (and, when a secret store is set, the store patch) actually
// matched a base resource — so a base without a VirtualService fails loudly
// instead of silently deploying every tenant on the base host. Overlays this
// package previously generated but the model no longer declares are removed;
// overlays it did not generate are never touched.
func Generate(root string, model *Model) (*Result, error) {
	base := filepath.Join(root, filepath.FromSlash(model.Base))
	if info, err := os.Stat(base); err != nil {
		return nil, fmt.Errorf("tenant model base %q: %w", model.Base, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("tenant model base %q is not a directory", model.Base)
	}
	desired := make(map[string]struct{}, len(model.Tenants))
	result := &Result{Written: make([]string, 0, len(model.Tenants))}
	for i := range model.Tenants {
		tenant := &model.Tenants[i]
		overlayDir := filepath.Join(root, "overlays", tenant.Overlay())
		relBase, err := filepath.Rel(overlayDir, base)
		if err != nil {
			return nil, fmt.Errorf("locate base from overlay %q: %w", tenant.Overlay(), err)
		}
		if err = prepareOverlayDir(overlayDir, tenant.Overlay()); err != nil {
			return nil, err
		}
		document, err := renderOverlay(filepath.ToSlash(relBase), tenant)
		if err != nil {
			return nil, fmt.Errorf("render overlay %q: %w", tenant.Overlay(), err)
		}
		if err = os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), document, 0o644); err != nil { //nolint:gosec
			return nil, fmt.Errorf("write overlay %q: %w", tenant.Overlay(), err)
		}
		if err = os.WriteFile(filepath.Join(overlayDir, generatedMarker), []byte(ModelSchema+"\n"), 0o644); err != nil { //nolint:gosec
			return nil, fmt.Errorf("mark overlay %q: %w", tenant.Overlay(), err)
		}
		if err = verifyOverlay(overlayDir, tenant); err != nil {
			return nil, err
		}
		desired[tenant.Overlay()] = struct{}{}
		result.Written = append(result.Written, tenant.Overlay())
	}
	removed, err := reconcileOrphans(root, desired)
	if err != nil {
		return nil, err
	}
	result.Removed = removed
	sort.Strings(result.Written)
	return result, nil
}

// prepareOverlayDir readies a clean overlay directory. A directory this package
// previously generated is cleared so no stale resources survive a regeneration.
// A non-empty directory it did not generate is left intact and reported as an
// error rather than silently overwritten.
func prepareOverlayDir(dir, name string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0o755)
		}
		return fmt.Errorf("inspect overlay %q: %w", name, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("overlay path %q exists and is not a directory", name)
	}
	if !isGenerated(dir) {
		empty, err := isEmptyDir(dir)
		if err != nil {
			return err
		}
		if !empty {
			return fmt.Errorf("overlay %q already exists and was not generated by codefly; refusing to overwrite", name)
		}
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear overlay %q: %w", name, err)
	}
	return os.MkdirAll(dir, 0o755)
}

// reconcileOrphans removes overlay directories this package generated whose
// tenant is no longer in the model. It never removes overlays it did not
// generate (for example hand-authored environment overlays), which lack the
// marker.
func reconcileOrphans(root string, desired map[string]struct{}) ([]string, error) {
	overlays := filepath.Join(root, "overlays")
	entries, err := os.ReadDir(overlays)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read overlays: %w", err)
	}
	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if _, keep := desired[name]; keep {
			continue
		}
		dir := filepath.Join(overlays, name)
		if !isGenerated(dir) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("remove stale overlay %q: %w", name, err)
		}
		removed = append(removed, name)
	}
	sort.Strings(removed)
	return removed, nil
}

func isGenerated(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, generatedMarker))
	return err == nil
}

func isEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read overlay directory: %w", err)
	}
	return len(entries) == 0, nil
}

// verifyOverlay builds the overlay through Kustomize and confirms the tenant's
// host patch matched a VirtualService and, when set, the secret-store patch
// matched an ExternalSecret. Kustomize silently drops patches whose target
// matches nothing, so without this check a base missing those resources would
// generate and deploy with the tenant's host/store never applied.
func verifyOverlay(dir string, tenant *Tenant) error {
	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	resMap, err := kustomizer.Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return fmt.Errorf("build overlay %q: %w", tenant.Overlay(), err)
	}
	rendered, err := resMap.AsYaml()
	if err != nil {
		return fmt.Errorf("encode overlay %q: %w", tenant.Overlay(), err)
	}
	hostApplied := false
	storeApplied := tenant.SecretStore == ""
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			break
		}
		switch document["kind"] {
		case kindVirtualService:
			if virtualServiceHasHost(document, tenant.Host) {
				hostApplied = true
			}
		case kindExternalSecret:
			if externalSecretStore(document) == tenant.SecretStore {
				storeApplied = true
			}
		}
	}
	if !hostApplied {
		return fmt.Errorf("overlay %q: base defines no VirtualService for host %q; the tenant host patch would be silently dropped", tenant.Overlay(), tenant.Host)
	}
	if !storeApplied {
		return fmt.Errorf("overlay %q: secret-store %q is set but the base defines no ExternalSecret; the store patch would be silently dropped", tenant.Overlay(), tenant.SecretStore)
	}
	return nil
}

func virtualServiceHasHost(document map[string]any, host string) bool {
	spec, _ := document["spec"].(map[string]any)
	hosts, _ := spec["hosts"].([]any)
	for _, raw := range hosts {
		if value, ok := raw.(string); ok && value == host {
			return true
		}
	}
	return false
}

func externalSecretStore(document map[string]any) string {
	spec, _ := document["spec"].(map[string]any)
	ref, _ := spec["secretStoreRef"].(map[string]any)
	name, _ := ref["name"].(string)
	return name
}

type kustomization struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Resources  []string `yaml:"resources"`
	Patches    []patch  `yaml:"patches"`
}

type patch struct {
	Target target `yaml:"target"`
	Patch  string `yaml:"patch"`
}

type target struct {
	Kind string `yaml:"kind"`
}

func renderOverlay(relBase string, tenant *Tenant) ([]byte, error) {
	hostPatch, err := jsonPatch(map[string]any{
		"op":    "replace",
		"path":  "/spec/hosts",
		"value": []string{tenant.Host},
	})
	if err != nil {
		return nil, err
	}
	document := kustomization{
		APIVersion: "kustomize.config.k8s.io/v1beta1",
		Kind:       "Kustomization",
		Resources:  []string{relBase},
		Patches: []patch{
			{Target: target{Kind: kindVirtualService}, Patch: hostPatch},
		},
	}
	if tenant.SecretStore != "" {
		storePatch, err := jsonPatch(map[string]any{
			"op":    "replace",
			"path":  "/spec/secretStoreRef/name",
			"value": tenant.SecretStore,
		})
		if err != nil {
			return nil, err
		}
		document.Patches = append(document.Patches, patch{
			Target: target{Kind: kindExternalSecret}, Patch: storePatch,
		})
	}
	return yaml.Marshal(document)
}

// jsonPatch renders a single JSON6902 operation as the block-scalar body
// Kustomize expects in a patch entry.
func jsonPatch(op map[string]any) (string, error) {
	data, err := yaml.Marshal([]map[string]any{op})
	if err != nil {
		return "", err
	}
	return string(data), nil
}
