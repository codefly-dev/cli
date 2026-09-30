package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"gopkg.in/yaml.v3"
)

// RenderedServiceSecret is one remote secret a rendered environment's
// ExternalSecrets read: the store they resolve through, the remote key, and every
// property some rendered service reads from it. It is what the environment's
// secret store must hold for the render to materialize — read back from the
// render itself, because the render is what the cluster applies.
type RenderedServiceSecret struct {
	Store     environments.EnvironmentSecretStoreReference
	Namespace string
	RemoteKey string
	// Services are the module-qualified uniques whose ExternalSecret reads this
	// remote key, sorted.
	Services []string
	// Properties are the properties read from the remote key, sorted by property
	// name. An empty property is the whole remote value, for a store of bare
	// scalars.
	Properties []RenderedSecretProperty
}

// RenderedSecretProperty is one property of one remote key, and the secret keys
// the rendered ExternalSecrets read out of it.
//
// The two are not the same string. The property is only where a value is
// filed in the store; the key is what the value IS — the CODEFLY__… name core
// gives a configuration value, which is what a federation derivation, a
// `service-secrets.generate` declaration and one configuration value shared by
// several services are all named by. An environment that files a key under a
// human-named property (`property: postgres_user`) makes them differ, and
// anything that reads the property as if it were the key then recognizes none
// of the three and reports every value as one to type by hand.
type RenderedSecretProperty struct {
	Property string
	// Keys are the secret keys read from this property (an ExternalSecret's
	// data[].secretKey), sorted and de-duplicated. Two services reading one
	// property under one key give one entry; two different keys are kept, and
	// whoever resolves them says what a disagreement means.
	Keys []string
	// Readers retain the association between a key and the service reading it.
	// Services at the remote-document level are insufficient for derivation.
	Readers []RenderedSecretReader
}

// RenderedSecretReader is an actual service/key binding from an ExternalSecret.
type RenderedSecretReader struct {
	Service string
	Key     string
}

// RenderedEnvironment is every rendered module's secret requirements for one
// environment.
type RenderedEnvironment struct {
	Secrets []RenderedServiceSecret
	// Modules are the rendered modules whose render targets the environment.
	Modules []string
	// Skipped are rendered modules whose render targets another environment:
	// their ExternalSecrets are not this environment's.
	Skipped []string
}

// RenderedServiceSecrets reads the ExternalSecrets `deploy gitops render`
// projected into every module rendered under the workspace for environment,
// grouped by remote key. A module whose inventory records another environment is
// skipped rather than read: `deployments/modules/<module>` holds one render, and
// the overlay of a different environment would name that environment's keys.
func RenderedServiceSecrets(workspaceDir, environment string) (RenderedEnvironment, error) {
	modulesRoot := filepath.Join(workspaceDir, "deployments", "modules")
	entries, err := os.ReadDir(modulesRoot)
	if errors.Is(err, os.ErrNotExist) {
		return RenderedEnvironment{}, fmt.Errorf("no rendered modules under %s: run `codefly deploy gitops render <module> --env %s` first", modulesRoot, environment)
	}
	if err != nil {
		return RenderedEnvironment{}, err
	}
	var rendered RenderedEnvironment
	byKey := map[string]*RenderedServiceSecret{}
	for _, entry := range entries {
		// A dot-directory is a render's staging area, never a rendered module.
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		root := filepath.Join(modulesRoot, entry.Name())
		inventory, err := LoadInventory(root)
		if err != nil {
			return RenderedEnvironment{}, fmt.Errorf("module %s: %w", entry.Name(), err)
		}
		if inventory.Environment != environment {
			rendered.Skipped = append(rendered.Skipped, inventory.Module)
			continue
		}
		rendered.Modules = append(rendered.Modules, inventory.Module)
		for index := range inventory.Units {
			unit := &inventory.Units[index]
			if unit.Kind != "service" || unit.Path == "" {
				continue
			}
			projection, err := readProjectedSecret(root, projectedSecretPath(unit, environment), &inventory)
			if err != nil {
				return RenderedEnvironment{}, fmt.Errorf("service %s/%s: %w", unit.Module, unit.Name, err)
			}
			if projection == nil {
				continue
			}
			unique := unit.Module + "/" + unit.Name
			for _, data := range projection.Spec.Data {
				if data.RemoteRef.Key == "" {
					return RenderedEnvironment{}, fmt.Errorf("service %s reads %s from an empty remote key", unique, data.SecretKey)
				}
				if data.SecretKey == "" {
					return RenderedEnvironment{}, fmt.Errorf("service %s reads %s#%s into no secret key: nothing names what that value is",
						unique, data.RemoteRef.Key, data.RemoteRef.Property)
				}
				secret, seen := byKey[data.RemoteRef.Key]
				store := environments.EnvironmentSecretStoreReference{Name: projection.Spec.SecretStoreRef.Name, Kind: projection.Spec.SecretStoreRef.Kind}
				switch {
				case !seen:
					secret = &RenderedServiceSecret{Store: store, Namespace: projection.Metadata.Namespace, RemoteKey: data.RemoteRef.Key}
					byKey[data.RemoteRef.Key] = secret
				case secret.Store != store:
					return RenderedEnvironment{}, fmt.Errorf("remote key %s is read through two stores (%s and %s)", data.RemoteRef.Key, secret.Store.Name, store.Name)
				case store.Kind == "SecretStore" && secret.Namespace != projection.Metadata.Namespace:
					// A SecretStore is namespaced, so the same name in two namespaces is
					// two objects, resolving through two backends. Grouping by remote key
					// alone would collapse them and leave whichever namespace was read
					// first deciding the backend for both.
					return RenderedEnvironment{}, fmt.Errorf("remote key %s is read through the namespaced SecretStore %s in namespaces %s and %s, which are two different stores",
						data.RemoteRef.Key, store.Name, secret.Namespace, projection.Metadata.Namespace)
				}
				if !slices.Contains(secret.Services, unique) {
					secret.Services = append(secret.Services, unique)
				}
				index := slices.IndexFunc(secret.Properties, func(property RenderedSecretProperty) bool {
					return property.Property == data.RemoteRef.Property
				})
				if index < 0 {
					secret.Properties = append(secret.Properties, RenderedSecretProperty{Property: data.RemoteRef.Property})
					index = len(secret.Properties) - 1
				}
				reader := RenderedSecretReader{Service: unique, Key: data.SecretKey}
				if !slices.Contains(secret.Properties[index].Readers, reader) {
					secret.Properties[index].Readers = append(secret.Properties[index].Readers, reader)
				}
				if !slices.Contains(secret.Properties[index].Keys, data.SecretKey) {
					secret.Properties[index].Keys = append(secret.Properties[index].Keys, data.SecretKey)
				}
			}
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		secret := byKey[key]
		sort.Strings(secret.Services)
		sort.Slice(secret.Properties, func(i, j int) bool { return secret.Properties[i].Property < secret.Properties[j].Property })
		for index := range secret.Properties {
			sort.Strings(secret.Properties[index].Keys)
			sort.Slice(secret.Properties[index].Readers, func(i, j int) bool {
				a, b := secret.Properties[index].Readers[i], secret.Properties[index].Readers[j]
				if a.Service != b.Service {
					return a.Service < b.Service
				}
				return a.Key < b.Key
			})
		}
		rendered.Secrets = append(rendered.Secrets, *secret)
	}
	sort.Strings(rendered.Modules)
	sort.Strings(rendered.Skipped)
	return rendered, nil
}

// projectedSecretPath is where the render put a unit's ExternalSecret. A regular
// service's is projected into its environment overlay (projectServiceSecrets); a
// managed service has no overlay of its own — its bundle is assembled from the
// bootstrap Jobs and the projection together (retainManagedBundle), and the
// projection is the bundle's base, which every environment's overlay includes.
// Reading only the overlay path left every remote key an environment's
// managed-services secret-references name out of the plan entirely.
func projectedSecretPath(unit *InventoryUnit, environment string) string {
	if unit.Managed {
		return unit.Path + "/base/external-secret.yaml"
	}
	return unit.Path + "/overlays/" + environment + "/external-secret.yaml"
}

// readProjectedSecret decodes the ExternalSecret the render projected, or nil
// when the service references no secret and so has none. The file must be the
// one the render recorded: an ExternalSecret edited after the render names keys
// the render never derived, and seeding a store from it would hide the edit
// rather than surface it. A projection the inventory records but that is no
// longer on disk is refused for the same reason and a worse one — deleting it
// takes its keys out of the plan silently, and a federation credential whose
// only carrier left that way is one the registrar would then be handed a digest
// of and no service the plaintext.
func readProjectedSecret(root, relative string, inventory *Inventory) (*externalSecret, error) {
	recorded := slices.IndexFunc(inventory.Files, func(file InventoryFile) bool { return file.Path == relative })
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if errors.Is(err, os.ErrNotExist) {
		if recorded >= 0 {
			return nil, fmt.Errorf("%s is recorded in %s but is no longer there: re-render instead of deleting it", relative, InventoryFilename)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if recorded < 0 || inventory.Files[recorded].SHA256 != digest {
		return nil, fmt.Errorf("%s is not the file the render recorded in %s: re-render instead of editing it", relative, InventoryFilename)
	}
	var projection externalSecret
	if err := yaml.Unmarshal(data, &projection); err != nil {
		return nil, fmt.Errorf("decode %s: %w", relative, err)
	}
	if projection.Kind != kindExternalSecret {
		return nil, fmt.Errorf("%s is a %q, not an %s", relative, projection.Kind, kindExternalSecret)
	}
	return &projection, nil
}
