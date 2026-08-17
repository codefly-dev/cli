package gitops

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// externalSecretAPIVersion is the External Secrets Operator API the promotable
// bundle already understands: render.go whitelists external-secrets.io secretKey
// references and pkg/tenants patches an ExternalSecret's store per tenant.
const (
	externalSecretAPIVersion = "external-secrets.io/v1"
	kindExternalSecret       = "ExternalSecret"
	// secretRefreshInterval keeps the External Secrets Operator re-reading the
	// remote store on a fixed cadence so a rotated vault value propagates into the
	// in-cluster Secret. Omitting it lets the field default to "0" on some ESO
	// installs, which syncs once and never refreshes — silently defeating rotation.
	secretRefreshInterval = "1h"

	kindSecretStore        = "SecretStore"
	kindClusterSecretStore = "ClusterSecretStore"
)

// externalSecretStoreKinds are the only kinds an ExternalSecret's secretStoreRef
// accepts. An environment declaration naming a backend type ("azure-keyvault")
// instead of one of these renders a manifest that passes codefly's render checks
// but is rejected by ESO admission after promotion, so it is caught here.
var externalSecretStoreKinds = map[string]struct{}{
	kindSecretStore:        {},
	kindClusterSecretStore: {},
}

type externalSecret struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   externalSecretMeta `yaml:"metadata"`
	Spec       externalSecretSpec `yaml:"spec"`
}

type externalSecretMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type externalSecretSpec struct {
	RefreshInterval string                 `yaml:"refreshInterval"`
	SecretStoreRef  externalSecretStoreRef `yaml:"secretStoreRef"`
	Target          externalSecretTarget   `yaml:"target"`
	Data            []externalSecretData   `yaml:"data"`
}

type externalSecretStoreRef struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
}

type externalSecretTarget struct {
	Name string `yaml:"name"`
}

type externalSecretData struct {
	SecretKey string               `yaml:"secretKey"`
	RemoteRef externalSecretRemote `yaml:"remoteRef"`
}

type externalSecretRemote struct {
	Key string `yaml:"key"`
}

// managedSecretProjection renders the ExternalSecret that materializes a managed
// service's secret-<service> from a remote secret store. It is the missing joint
// of the KV→secretKeyRef chain: an environment declares where a managed service's
// secrets live (EnvironmentManagedSecretReference), the operator writes the real
// values into that store once, and this projection copies the declared remote
// keys into the in-cluster Secret the promotable manifests already reference — no
// secret value ever entering git, state, or manifests.
//
// Every reference of one managed service must resolve through the same store: an
// ExternalSecret owns a single target Secret, so mixing stores would silently
// drop all but one store's keys.
func managedSecretProjection(service, namespace string, refs []resources.EnvironmentManagedSecretReference) (*externalSecret, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	store := refs[0].SecretStore
	data := make([]externalSecretData, 0, len(refs))
	for _, ref := range refs {
		if ref.Name == "" || ref.RemoteKey == "" {
			return nil, fmt.Errorf("service %q secret reference requires both name and remote-key", service)
		}
		if ref.SecretStore != store {
			return nil, fmt.Errorf("service %q secret references resolve through more than one store", service)
		}
		data = append(data, externalSecretData{
			SecretKey: ref.Name,
			RemoteRef: externalSecretRemote{Key: ref.RemoteKey},
		})
	}
	return externalSecretProjection(service, namespace, store, data)
}

// serviceSecretProjection renders the ExternalSecret that materializes a regular
// (app) service's secret-<service> from the environment's declared service secret
// store — the app-service half of the same KV to secretKeyRef chain. Unlike a
// managed service, whose remote keys are enumerated in the environment, an app
// service's keys are exactly the ones its promotable manifests already reference
// via non-optional secretKeyRefs, discovered from the rendered tree. Each key
// resolves to the store path "<service>/<key>" unless the environment overrides it.
func serviceSecretProjection(service, namespace string, secrets *resources.EnvironmentServiceSecrets, keys []string) (*externalSecret, error) {
	if secrets == nil || len(keys) == 0 {
		return nil, nil
	}
	mapping := secrets.Services[service]
	// A service may resolve from a different store than the environment default,
	// mirroring EnvironmentManagedSecretReference's per-reference store; without
	// this the per-service secret-store declaration would load, validate, and then
	// be silently projected against the environment-wide store.
	store := secrets.SecretStore
	if mapping.SecretStore != nil {
		store = *mapping.SecretStore
	}
	data := make([]externalSecretData, 0, len(keys))
	for _, key := range keys {
		remoteKey := mapping.RemoteKeys[key]
		if remoteKey == "" {
			remoteKey = service + "/" + key
		}
		data = append(data, externalSecretData{
			SecretKey: key,
			RemoteRef: externalSecretRemote{Key: remoteKey},
		})
	}
	return externalSecretProjection(service, namespace, store, data)
}

// externalSecretProjection assembles the ExternalSecret shared by the managed- and
// app-service projections: same target (secret-<service>), same rotation cadence,
// same store-kind guard. An environment declaration naming a backend type
// ("azure-keyvault") instead of SecretStore/ClusterSecretStore renders a manifest
// that passes codefly's checks but is rejected by ESO admission, so it fails here.
func externalSecretProjection(service, namespace string, store resources.EnvironmentSecretStoreReference, data []externalSecretData) (*externalSecret, error) {
	if namespace == "" {
		return nil, fmt.Errorf("service %q declares secret references but its environment has no namespace", service)
	}
	if store.Name == "" || store.Kind == "" {
		return nil, fmt.Errorf("service %q secret store requires both name and kind", service)
	}
	if _, ok := externalSecretStoreKinds[store.Kind]; !ok {
		return nil, fmt.Errorf("service %q secret store kind %q must be SecretStore or ClusterSecretStore", service, store.Kind)
	}
	target := "secret-" + service
	return &externalSecret{
		APIVersion: externalSecretAPIVersion,
		Kind:       kindExternalSecret,
		Metadata:   externalSecretMeta{Name: target, Namespace: namespace},
		Spec: externalSecretSpec{
			RefreshInterval: secretRefreshInterval,
			SecretStoreRef:  externalSecretStoreRef{Name: store.Name, Kind: store.Kind},
			Target:          externalSecretTarget{Name: target},
			Data:            data,
		},
	}, nil
}

// projectServiceSecrets writes a regular service's ExternalSecret projection into
// its environment overlay and adds it to that overlay's kustomization, so the
// store binding stays environment-scoped like the overlay it lives in. The keys it
// projects are exactly the ones the service's rendered promotable manifests already
// reference from secret-<service>, so the projection covers precisely what the
// Secret must hold. It is a no-op — reporting false — when the environment declares
// no service secret store or the service references no such Secret.
func projectServiceSecrets(serviceRoot, service, environment, namespace string, secrets *resources.EnvironmentServiceSecrets) (bool, error) {
	if secrets == nil {
		return false, nil
	}
	keys, err := serviceSecretKeys(serviceRoot, service)
	if err != nil {
		return false, err
	}
	projection, err := serviceSecretProjection(service, namespace, secrets, keys)
	if err != nil {
		return false, err
	}
	if projection == nil {
		return false, nil
	}
	overlay := filepath.Join(serviceRoot, "overlays", environment)
	if info, statErr := os.Stat(overlay); statErr != nil || !info.IsDir() {
		return false, fmt.Errorf("service %q references secret-%s but has no %q environment overlay to project it into", service, service, environment)
	}
	projected, err := yaml.Marshal(projection)
	if err != nil {
		return false, err
	}
	// The projection is an ExternalSecret — a reference to remote keys, never a
	// secret value — so it stays a world-readable manifest like its siblings.
	if err := os.WriteFile(filepath.Join(overlay, "external-secret.yaml"), projected, 0o644); err != nil { //nolint:gosec
		return false, err
	}
	return true, addKustomizationResource(overlay, "external-secret.yaml")
}

// serviceSecretKeys collects, sorted and de-duplicated, the keys a service's
// rendered manifests reference from its secret-<service> Secret via secretKeyRef.
func serviceSecretKeys(root, service string) ([]string, error) {
	secretName := "secret-" + service
	seen := map[string]struct{}{}
	err := walkRegularFiles(root, func(path, relative string, _ os.FileInfo) error {
		extension := strings.ToLower(filepath.Ext(relative))
		if extension != yamlExtension && extension != ymlExtension && extension != jsonExtension {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests, _, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		for _, item := range manifests {
			collectSecretKeyRefs(item.value, secretName, seen)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func collectSecretKeyRefs(value any, secretName string, seen map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		if ref, ok := typed["secretKeyRef"].(map[string]any); ok {
			if name, _ := ref["name"].(string); name == secretName {
				if key, _ := ref["key"].(string); key != "" {
					seen[key] = struct{}{}
				}
			}
		}
		for _, child := range typed {
			collectSecretKeyRefs(child, secretName, seen)
		}
	case []any:
		for _, child := range typed {
			collectSecretKeyRefs(child, secretName, seen)
		}
	}
}

// addKustomizationResource appends a resource to an existing overlay
// kustomization, preserving its other fields.
func addKustomizationResource(directory, resource string) error {
	path, err := kustomizationPath(directory)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	document := map[string]any{}
	if err = yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	existing, _ := document["resources"].([]any)
	document["resources"] = append(existing, resource)
	updated, err := yaml.Marshal(document)
	if err != nil {
		return err
	}
	// A kustomization is a plain manifest index, world-readable like its siblings.
	return os.WriteFile(path, updated, 0o644) //nolint:gosec
}

func kustomizationPath(directory string) (string, error) {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", kindKustomization} {
		path := filepath.Join(directory, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("no kustomization in %s", directory)
}
