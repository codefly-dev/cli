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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModelSchema is the accepted schemaVersion for a tenant model file.
const ModelSchema = "codefly.dev/tenant-model/v1"

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

// Generate writes overlays/<tenant>-<cloud>/kustomization.yaml under root for
// every tenant in the model, layering the shared base and patching the
// VirtualService host and (when set) the External Secrets store reference. It
// returns the overlay directory names it wrote, sorted. Existing overlay
// kustomizations are overwritten; directories for tenants no longer in the
// model are left untouched.
func Generate(root string, model *Model) ([]string, error) {
	base := filepath.Join(root, filepath.FromSlash(model.Base))
	info, err := os.Stat(base)
	if err != nil {
		return nil, fmt.Errorf("tenant model base %q: %w", model.Base, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("tenant model base %q is not a directory", model.Base)
	}
	written := make([]string, 0, len(model.Tenants))
	for i := range model.Tenants {
		tenant := &model.Tenants[i]
		overlayDir := filepath.Join(root, "overlays", tenant.Overlay())
		if err := os.MkdirAll(overlayDir, 0o755); err != nil {
			return nil, fmt.Errorf("create overlay %q: %w", tenant.Overlay(), err)
		}
		relBase, err := filepath.Rel(overlayDir, base)
		if err != nil {
			return nil, fmt.Errorf("locate base from overlay %q: %w", tenant.Overlay(), err)
		}
		document, err := renderOverlay(filepath.ToSlash(relBase), tenant)
		if err != nil {
			return nil, fmt.Errorf("render overlay %q: %w", tenant.Overlay(), err)
		}
		if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), document, 0o644); err != nil { //nolint:gosec
			return nil, fmt.Errorf("write overlay %q: %w", tenant.Overlay(), err)
		}
		written = append(written, tenant.Overlay())
	}
	sort.Strings(written)
	return written, nil
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
			{Target: target{Kind: "VirtualService"}, Patch: hostPatch},
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
			Target: target{Kind: "ExternalSecret"}, Patch: storePatch,
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
