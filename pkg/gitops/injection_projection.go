package gitops

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"gopkg.in/yaml.v3"
)

// serviceInjection is what the CLI itself derives for one rendered service,
// beside what its builder rendered: the composed-solution federation carriers
// (solutionrun.DerivedDeployInputs) and the self-endpoint carrier
// (orchestration.SelfEndpointEnvironmentVariables).
type serviceInjection = solutionrun.ServiceInjection

// envEntryName is the name field of a container env entry and a secretKeyRef.
const envEntryName = "name"

// renderInjections is every derived injection of one render, keyed by service
// unique.
type renderInjections map[string]serviceInjection

func (injections renderInjections) forService(module, service string) serviceInjection {
	return injections[resources.ServiceUnique(module, service)]
}

// deriveRenderInjections joins the federation carriers of the workspace's
// composed solutions with the self-endpoint carriers of the services the render
// deployed. Both are public/secret splits of the same shape; a key is never
// derived by both, so the join is a plain union.
func deriveRenderInjections(ctx context.Context, workspace *resources.Workspace, selfEndpoints map[string]map[string]string, sink orchestration.OutputSink) (renderInjections, error) {
	federation, err := solutionrun.DerivedDeployInputs(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if sink != nil {
		for _, note := range federation.Notes {
			if note.Warning {
				sink.Info("warning: %s", note.Message)
			} else {
				sink.Info("%s", note.Message)
			}
		}
	}
	injections := renderInjections{}
	for unique, injection := range federation.Services {
		injections[unique] = injection
	}
	for unique, variables := range selfEndpoints {
		injection := injections[unique]
		if injection.Public == nil {
			injection.Public = map[string]string{}
		}
		for key, value := range variables {
			injection.Public[key] = value
		}
		injections[unique] = injection
	}
	return injections, nil
}

// validateInjectionClassification refuses a public value whose key core
// classifies as credential-bearing. The federation's public carriers name routes
// and prefixes, never credentials, so a sensitive public key means a secret was
// misfiled — and a render is committed to a repository, so the only safe answer
// is to stop. Self-endpoint carriers are the one exception, because their names
// embed module, service and endpoint names ("auth-gateway") that trip the broad
// markers exactly as the builder's own CODEFLY__ENDPOINT__ carriers do; their
// values are addresses, and an address carrying credentials is refused instead.
func validateInjectionClassification(service string, injection serviceInjection) error {
	for key, value := range injection.Public {
		if strings.HasPrefix(key, resources.SelfEndpointPrefix+"__") {
			if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
				return fmt.Errorf("service %q self endpoint %s carries credentials in its address", service, key)
			}
			continue
		}
		if resources.IsSensitiveKey(key) {
			return fmt.Errorf("service %q would render the credential-named key %s as a plain value; it must be a secret reference", service, key)
		}
	}
	for key := range injection.Secrets {
		if _, public := injection.Public[key]; public {
			return fmt.Errorf("service %q derives %s as both a value and a secret", service, key)
		}
	}
	return nil
}

// projectServiceInjection writes a service's derived carriers into its rendered
// tree: public values into the ConfigMap its container loads its configuration
// from, secrets as secretKeyRefs on secret-<service>. The ExternalSecret that
// materializes secret-<service> from the environment's store is projected
// afterwards from every secretKeyRef in the tree (projectServiceSecrets), so a
// referenced key resolves through exactly the same store mapping as the
// builder's own secret references — and no secret value is ever written.
//
// A secret with no store to deliver it is refused rather than rendered: a
// dangling secretKeyRef keeps the pod in CreateContainerConfigError, and the
// only other way to carry the value would be plaintext in the tree.
func projectServiceInjection(ctx context.Context, root, service string, env *environments.Environment, injection serviceInjection) error {
	if len(injection.Public) == 0 && len(injection.Secrets) == 0 {
		return nil
	}
	if err := validateInjectionClassification(service, injection); err != nil {
		return err
	}
	if len(injection.Secrets) > 0 && env.ServiceSecrets == nil {
		return fmt.Errorf(
			"service %q needs %s, which a render carries only as references into the environment's secret store, and environment %q declares no service secret store",
			service, strings.Join(sortedKeys(injection.Secrets), ", "), env.Name)
	}
	files, err := configurationSources(root, env.Name)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	var documents []manifest
	for path, fileDocuments := range files {
		paths = append(paths, path)
		documents = append(documents, fileDocuments...)
	}
	sort.Strings(paths)
	configMaps, err := indexConfigurationMaps(documents)
	if err != nil {
		return err
	}
	// Maps are shared between the index and the decoded documents, so writing
	// into a bound ConfigMap's data mutates the document it will be encoded from.
	// Only the files a binding changes are re-encoded: the rest keep their
	// bytes, comments included.
	changed := map[uintptr]bool{}
	configMapDocuments := map[namespacedName]map[string]any{}
	for _, document := range documents {
		if document.group == "" && document.kind == "ConfigMap" {
			key := namespacedName{namespace: metadataString(document.value, "namespace"), name: metadataString(document.value, "name")}
			configMapDocuments[key] = document.value
		}
	}
	matched := 0
	for _, document := range documents {
		spec, ok := podSpec(document)
		if !ok {
			continue
		}
		namespace := metadataString(document.value, "namespace")
		for _, candidate := range sliceField(spec, "containers") {
			container, ok := candidate.(map[string]any)
			if !ok {
				return fmt.Errorf("service %q has an invalid container", service)
			}
			boundService, err := configMaps.service(container, namespace)
			if err != nil {
				return err
			}
			if boundService != service {
				continue
			}
			matched++
			// A container that loads no Codefly ConfigMap (a gateway image that is
			// not a Codefly SDK process) reads no carrier: offered ones are skipped
			// for it exactly as for an unclaimed service; required ones refuse.
			if _, loads := boundConfigMap(container, namespace, service, configMaps); !loads && offeredOnly(injection) {
				continue
			}
			if len(injection.Public) > 0 {
				bound, ok := boundConfigMap(container, namespace, service, configMaps)
				if !ok {
					return fmt.Errorf("service %q has no ConfigMap bound through envFrom to render %s into", service, strings.Join(sortedKeys(injection.Public), ", "))
				}
				configMap := configMapDocuments[bound]
				data := mapField(configMap, "data")
				if data == nil {
					data = map[string]any{}
					configMap["data"] = data
				}
				if err := bindPublicInjection(container, data, service, injection.Public); err != nil {
					return err
				}
				changed[reflect.ValueOf(configMap).Pointer()] = true
			}
			if len(injection.Secrets) > 0 {
				if err := bindSecretInjection(container, service, injection.Secrets); err != nil {
					return err
				}
				changed[reflect.ValueOf(document.value).Pointer()] = true
			}
		}
	}
	if matched == 0 {
		// See offeredOnly: an unclaimed service is a vendor image that reads no
		// Codefly carrier, so offered carriers are skipped; required ones refuse.
		if offeredOnly(injection) {
			return nil
		}
		return fmt.Errorf("service %q derives %s but no rendered container declares %s=%s",
			service, strings.Join(append(sortedKeys(injection.Public), sortedKeys(injection.Secrets)...), ", "), resources.ServicePrefix, service)
	}
	return writeChangedConfigurationFiles(ctx, paths, files, changed)
}

// offeredOnly reports whether every carrier a service derives is one offered to
// any Codefly-aware process of it — its own reachable address, or the consumed
// module's identity every service of that module receives — rather than one the
// render must deliver (a solution's consumed routes and registration secrets).
// A container that declares no CODEFLY__SERVICE (a vendor image: postgres,
// redis) reads none of them, so an unclaimed service may skip offered carriers.
func offeredOnly(injection serviceInjection) bool {
	offered := func(key string) bool {
		switch key {
		case "CODEFLY__MODULE_IDENTITY_PREFIX", "CODEFLY__MODULE_IDENTITY_SECRET", "CODEFLY__MODULE_REGISTRATION_SECRET":
			return true
		}
		return strings.HasPrefix(key, resources.SelfEndpointPrefix+"__")
	}
	for key := range injection.Public {
		if !offered(key) {
			return false
		}
	}
	for key := range injection.Secrets {
		if !offered(key) {
			return false
		}
	}
	return true
}

// writeChangedConfigurationFiles re-encodes only the files holding a document a
// binding changed; every other file keeps its bytes, comments included.
func writeChangedConfigurationFiles(ctx context.Context, paths []string, files map[string][]manifest, changed map[uintptr]bool) error {
	for _, path := range paths {
		if !slices.ContainsFunc(files[path], func(document manifest) bool {
			return changed[reflect.ValueOf(document.value).Pointer()]
		}) {
			continue
		}
		var out strings.Builder
		encoder := yaml.NewEncoder(&out)
		encoder.SetIndent(2)
		for _, document := range files[path] {
			if err := encoder.Encode(document.value); err != nil {
				return err
			}
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		if err := shared.WriteFileAtomic(ctx, path, []byte(out.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// boundConfigMap returns the ConfigMap a container loads its service identity
// from: the last unprefixed envFrom source declaring CODEFLY__SERVICE=service,
// since a later envFrom source wins.
func boundConfigMap(container map[string]any, namespace, service string, configMaps configurationMaps) (namespacedName, bool) {
	var bound namespacedName
	found := false
	for _, raw := range sliceField(container, "envFrom") {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if prefix, _ := entry["prefix"].(string); prefix != "" {
			continue
		}
		name, _ := mapField(entry, "configMapRef")["name"].(string)
		key := namespacedName{namespace: namespace, name: name}
		if data, exists := configMaps[key]; exists && data[resources.ServicePrefix] == service {
			bound, found = key, true
		}
	}
	return bound, found
}

// bindPublicInjection writes public values into the bound ConfigMap. A value the
// builder already rendered is kept when it agrees and refused when it does not,
// and a container env entry of the same name — which would shadow the ConfigMap
// — is refused: the derived value must be the one the process reads.
func bindPublicInjection(container map[string]any, data map[string]any, service string, values map[string]string) error {
	for _, item := range sliceField(container, "env") {
		entry, _ := item.(map[string]any)
		name, _ := entry["name"].(string)
		if _, derived := values[name]; derived {
			return fmt.Errorf("service %q container env entry %s would shadow the derived value in its ConfigMap", service, name)
		}
	}
	for _, key := range sortedKeys(values) {
		if existing, rendered := data[key]; rendered && existing != values[key] {
			return fmt.Errorf("service %q ConfigMap already carries %s=%v, which disagrees with the derived %q", service, key, existing, values[key])
		}
		data[key] = values[key]
	}
	return nil
}

// bindSecretInjection adds a non-optional secretKeyRef on secret-<service> for
// each derived secret. An existing entry of the same name must already be that
// exact reference: a literal, or a reference to another key or Secret, is a
// conflict the render refuses rather than overrides.
func bindSecretInjection(container map[string]any, service string, secrets map[string]string) error {
	if len(secrets) == 0 {
		return nil
	}
	existing := sliceField(container, "env")
	byName := map[string]map[string]any{}
	for _, item := range existing {
		entry, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("service %q has an invalid container environment entry", service)
		}
		name, _ := entry["name"].(string)
		byName[name] = entry
	}
	for _, key := range sortedKeys(secrets) {
		secretName := "secret-" + service
		reference := map[string]any{envEntryName: secretName, "key": secrets[key], "optional": false}
		if prior, exists := byName[key]; exists {
			ref := mapField(mapField(prior, "valueFrom"), "secretKeyRef")
			if ref == nil || ref[envEntryName] != secretName || ref["key"] != secrets[key] || ref["optional"] == true {
				return fmt.Errorf("service %q already renders %s other than as a reference to %s/%s", service, key, secretName, secrets[key])
			}
			continue
		}
		existing = append(existing, map[string]any{envEntryName: key, "valueFrom": map[string]any{"secretKeyRef": reference}})
	}
	container["env"] = existing
	return nil
}

// validateProjectedInjection checks the selected overlay, not only the files the
// projection wrote: a patch can drop a derived value, repoint a reference or
// shadow the ConfigMap after projection.
func validateProjectedInjection(root, service string, env *environments.Environment, injection serviceInjection) error {
	if len(injection.Public) == 0 && len(injection.Secrets) == 0 {
		return nil
	}
	documents, err := effectiveConfiguration(root, env.Name)
	if err != nil {
		return fmt.Errorf("build configuration overlay for %q: %w", service, err)
	}
	configMaps, err := indexConfigurationMaps(documents)
	if err != nil {
		return err
	}
	matched := 0
	for _, document := range documents {
		spec, ok := podSpec(document)
		if !ok {
			continue
		}
		namespace := metadataString(document.value, "namespace")
		for _, candidate := range sliceField(spec, "containers") {
			container, ok := candidate.(map[string]any)
			if !ok {
				return fmt.Errorf("service %q has an invalid effective container", service)
			}
			if bound, err := configMaps.service(container, namespace); err != nil || bound != service {
				continue
			}
			matched++
			if _, loads := boundConfigMap(container, namespace, service, configMaps); !loads && offeredOnly(injection) {
				continue
			}
			entries := map[string]map[string]any{}
			for _, item := range sliceField(container, "env") {
				entry, _ := item.(map[string]any)
				name, _ := entry["name"].(string)
				entries[name] = entry
			}
			if len(injection.Public) > 0 {
				bound, ok := boundConfigMap(container, namespace, service, configMaps)
				if !ok {
					return fmt.Errorf("service %q overlay unbinds the ConfigMap carrying its derived configuration", service)
				}
				for key, value := range injection.Public {
					if _, shadowed := entries[key]; shadowed || configMaps[bound][key] != value {
						return fmt.Errorf("service %q overlay drops or overrides derived configuration key %s", service, key)
					}
				}
			}
			for key, storeKey := range injection.Secrets {
				ref := mapField(mapField(entries[key], "valueFrom"), "secretKeyRef")
				if ref == nil || ref["name"] != "secret-"+service || ref["key"] != storeKey || ref["optional"] == true || entries[key]["value"] != nil {
					return fmt.Errorf("service %q overlay drops or overrides derived secret reference %s", service, key)
				}
			}
		}
	}
	if matched == 0 {
		// Mirrors projectServiceInjection: offered carriers were skipped there.
		if offeredOnly(injection) {
			return nil
		}
		return fmt.Errorf("service %q derived configuration binds no effective workload", service)
	}
	return nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
