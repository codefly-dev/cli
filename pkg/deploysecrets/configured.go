package deploysecrets

// The environment's own configuration files are a source of secret values, and
// this is the read half of the pair `codefly config set` writes.
//
// `codefly config set <group> KEY=VALUE --env <env>` writes
// `<scope>/configurations/<profile>/<group>.secret.env` — 0600, inside a 0700
// directory, refused when git would track it, never printed. Those files are
// what a local run reads. Until a plan could read them too, a value an operator
// had already stored there reached a deployed workload only by being typed into
// the store's console or `gcloud` by hand: exactly the hand write this verb
// exists to remove.
//
// Two decisions make this read safe to write from:
//
//   - Only `<group>.secret.env` is read. A reference-only manifest
//     (`<group>.secret.ref.env`) holds `op://…` URIs, which are resolved
//     against a backend at load; writing one into the store would store the
//     reference, not the value. `cmd/config.SecretConfigurationNames` selects
//     the one suffix, so the reader and the writer cannot drift apart on it.
//   - A key present with an empty value supplies nothing. A tracked placeholder
//     file — the committed shape that declares which keys an environment needs
//     without holding any — therefore keeps contributing keys and no values,
//     and a property it names is still reported as one the operator must
//     supply.
//
// The profile directory read is the one `codefly config set --env <env>` writes:
// the environment's ConfigurationProfileName, which is the first profile of its
// chain. The pair is deliberately symmetric — the reader reads the directory the
// writer writes — and that symmetry bounds it: a value an environment reaches
// only through a later profile of a chain, or through a profile DERIVATION
// (`derives-from`, which core layers per location), is not read here and its
// property is still reported as one the operator must supply. Nothing about such
// a value is written incorrectly; it is simply not a source yet.
//
// The secret key each value is indexed by is core's own encoding of the carrier
// (`resources.ConfigurationValueEnvironmentKey`), the same name the render
// writes into the projected ExternalSecret's `secretKey`. The two sides meet on
// core's encoding rather than on a string spelled here, so a group or key whose
// spelling core folds (`-` to `_`, lower to upper) matches whichever way it was
// written.

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/codefly-dev/cli/cmd/config"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// ConfiguredValue is one value the environment's own configuration holds for a
// secret key, with the file it came from. The value is unexported: a plan
// reports where a value comes from, never what it is.
type ConfiguredValue struct {
	// File is the configuration file holding it, relative to the workspace. It
	// is what `--dry-run` names as the source, and it is a path, never a value.
	File string

	value string
}

// ReadConfigured indexes every non-empty value the environment's own secret
// configuration files hold, by the secret key a render reads it under: the
// workspace's own `configurations/<profile>/*.secret.env` under the workspace
// carrier, and each service's under that service's carrier.
//
// A module that does not load is skipped rather than failing the read: it is
// not one this environment renders either, so it holds no key a plan is
// resolving. The same rule as the service listing a plan is given.
func ReadConfigured(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (map[string]ConfiguredValue, error) {
	profile, err := env.ConfigurationProfileName()
	if err != nil {
		return nil, fmt.Errorf("environment %s: %w", env.Name, err)
	}
	root := workspace.Dir()
	index := map[string]ConfiguredValue{}
	if err = readConfiguredScope(index, root, root, profile, resources.ConfigurationWorkspace); err != nil {
		return nil, err
	}
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			continue
		}
		for _, svcRef := range mod.ServiceReferences {
			svc, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: svcRef.Name, Module: mod.Name})
			if err != nil {
				continue
			}
			if err = readConfiguredScope(index, root, svc.Dir(), profile, resources.ServiceUnique(mod.Name, svcRef.Name)); err != nil {
				return nil, err
			}
		}
	}
	return index, nil
}

// readConfiguredScope adds one scope directory's secret configurations to the
// index, under origin — resources.ConfigurationWorkspace for the workspace, the
// `module/service` unique for a service.
//
// Two files resolving to one secret key with different values is refused by
// name. Core folds a configuration name into the carrier (`-` and `_` both
// become `_`, upper-cased), so `datasource-oauth.secret.env` and
// `datasource_oauth.secret.env` are two files whose keys are one carrier; a
// plan cannot choose between them, and choosing the one that sorted first would
// write a value by an accident of filesystem order.
func readConfiguredScope(index map[string]ConfiguredValue, root, scopeDir, profile, origin string) error {
	dir := filepath.Join(scopeDir, config.ConfigurationsDir, profile)
	names, err := config.SecretConfigurationNames(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		target := config.Target{ScopeDir: scopeDir, Profile: profile, Name: name, Secret: true}
		document, err := config.Load(target)
		if err != nil {
			return err
		}
		file := target.Display(root)
		for _, key := range document.Keys() {
			value, held := document.Value(key)
			if !held {
				// A tracked placeholder declares the key and holds no value.
				continue
			}
			secretKey := configuredSecretKey(origin, name, key)
			if previous, seen := index[secretKey]; seen && previous.value != value {
				return fmt.Errorf("%s and %s hold different values for the same secret key %s: one configuration value cannot be two, and their configuration names are one carrier to core",
					previous.File, file, secretKey)
			}
			index[secretKey] = ConfiguredValue{File: file, value: value}
		}
	}
	return nil
}

// configuredSecretKey is core's encoding of a secret configuration value's
// carrier: the name the render records as the ExternalSecret's `secretKey`, and
// the name every source in a plan is resolved by.
func configuredSecretKey(origin, group, key string) string {
	return resources.ConfigurationValueEnvironmentKey(
		&basev0.Configuration{Origin: origin},
		group,
		&basev0.ConfigurationValue{Key: key, Secret: true})
}

// configuredFor resolves the one configured value a property's secret keys
// name. A property read under two keys the configuration holds with different
// values is refused: the keys are one configuration value to the store, and
// whichever won would be an accident of ordering — the same refusal two remote
// keys holding one value differently already get.
func configuredFor(property gitops.RenderedSecretProperty, configured map[string]ConfiguredValue) (ConfiguredValue, bool, error) {
	var resolved ConfiguredValue
	var resolvedKey string
	for _, key := range property.Keys {
		candidate, ok := configured[key]
		if !ok {
			continue
		}
		if resolvedKey != "" && candidate.value != resolved.value {
			return ConfiguredValue{}, false, fmt.Errorf("the configuration holds different values for %s (%s) and %s (%s), which are one configuration value; resolve it before planning",
				resolvedKey, resolved.File, key, candidate.File)
		}
		resolved, resolvedKey = candidate, key
	}
	return resolved, resolvedKey != "", nil
}
