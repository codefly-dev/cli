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
	// Properties are the properties read from the remote key, sorted. An empty
	// property is the whole remote value, for a store of bare scalars.
	Properties []string
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
		for _, unit := range inventory.Units {
			if unit.Kind != "service" || unit.Path == "" {
				continue
			}
			relative := unit.Path + "/overlays/" + environment + "/external-secret.yaml"
			projection, err := readProjectedSecret(root, relative, &inventory)
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
				secret, seen := byKey[data.RemoteRef.Key]
				store := environments.EnvironmentSecretStoreReference{Name: projection.Spec.SecretStoreRef.Name, Kind: projection.Spec.SecretStoreRef.Kind}
				if !seen {
					secret = &RenderedServiceSecret{Store: store, Namespace: projection.Metadata.Namespace, RemoteKey: data.RemoteRef.Key}
					byKey[data.RemoteRef.Key] = secret
				} else if secret.Store != store {
					return RenderedEnvironment{}, fmt.Errorf("remote key %s is read through two stores (%s and %s)", data.RemoteRef.Key, secret.Store.Name, store.Name)
				}
				if !slices.Contains(secret.Services, unique) {
					secret.Services = append(secret.Services, unique)
				}
				if !slices.Contains(secret.Properties, data.RemoteRef.Property) {
					secret.Properties = append(secret.Properties, data.RemoteRef.Property)
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
		sort.Strings(secret.Properties)
		rendered.Secrets = append(rendered.Secrets, *secret)
	}
	sort.Strings(rendered.Modules)
	sort.Strings(rendered.Skipped)
	return rendered, nil
}

// readProjectedSecret decodes the ExternalSecret projectServiceSecrets wrote, or
// nil when the service references no secret and so has none. The file must be
// the one the render recorded: an ExternalSecret edited after the render names
// keys the render never derived, and seeding a store from it would hide the
// edit rather than surface it.
func readProjectedSecret(root, relative string, inventory *Inventory) (*externalSecret, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	recorded := slices.IndexFunc(inventory.Files, func(file InventoryFile) bool { return file.Path == relative })
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
