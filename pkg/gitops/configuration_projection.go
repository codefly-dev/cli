package gitops

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// projectConfigurationValues binds exact producer keys to the service container.
// Sidecars do not inherit the service's configuration or secret references.
func projectConfigurationValues(ctx context.Context, root, service string, env *environments.Environment) error {
	values := map[string]string{}
	if env.ServiceConfig != nil {
		values = env.ServiceConfig.Services[service].Values
	}
	refs := map[string]environments.EnvironmentSecretRemoteRef{}
	if env.ServiceSecrets != nil {
		refs = env.ServiceSecrets.Services[service].RemoteKeys
	}
	if len(values) == 0 && len(refs) == 0 {
		return nil
	}
	if err := env.Validate(); err != nil {
		return err
	}

	keys := make([]string, 0, len(values)+len(refs))
	for key := range values {
		keys = append(keys, key)
	}
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	files, err := configurationSources(root, env.Name)
	if err != nil {
		return err
	}
	var allDocuments []manifest
	paths := make([]string, 0, len(files))
	for path, documents := range files {
		paths = append(paths, path)
		allDocuments = append(allDocuments, documents...)
	}
	configMaps, err := indexConfigurationMaps(allDocuments)
	if err != nil {
		return err
	}
	sort.Strings(paths)
	pending := make(map[string][]byte)
	matched := 0
	for _, path := range paths {
		documents := files[path]
		changed := false
		for _, document := range documents {
			spec, ok := podSpec(document)
			if !ok {
				continue
			}
			for _, candidate := range sliceField(spec, "containers") {
				container, ok := candidate.(map[string]any)
				if !ok {
					return fmt.Errorf("service %q has an invalid container", service)
				}
				boundService, err := configMaps.service(container, metadataString(document.value, "namespace"))
				if err != nil {
					return err
				}
				if boundService != service {
					continue
				}
				matched++
				if err := bindContainerConfiguration(container, service, keys, values, refs); err != nil {
					return err
				}
				changed = true
			}
		}
		if !changed {
			continue
		}
		var out strings.Builder
		encoder := yaml.NewEncoder(&out)
		encoder.SetIndent(2)
		for _, document := range documents {
			if err := encoder.Encode(document.value); err != nil {
				return err
			}
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		pending[path] = []byte(out.String())
	}
	if matched == 0 {
		return fmt.Errorf("service %q declares configuration but no rendered container declares %s=%s", service, resources.ServicePrefix, service)
	}
	paths = paths[:0]
	for path := range pending {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := shared.WriteFileAtomic(ctx, path, pending[path], 0o644); err != nil {
			return err
		}
	}
	return nil
}

func bindContainerConfiguration(container map[string]any, service string, keys []string, values map[string]string, refs map[string]environments.EnvironmentSecretRemoteRef) error {
	existing := sliceField(container, "env")
	byName := make(map[string]int, len(existing))
	for i, item := range existing {
		entry, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("service %q has an invalid container environment entry", service)
		}
		name, _ := entry["name"].(string)
		if _, duplicate := byName[name]; duplicate {
			return fmt.Errorf("service %q repeats environment key %q", service, name)
		}
		byName[name] = i
	}
	for _, key := range keys {
		entry := map[string]any{"name": key}
		_, secret := refs[key]
		if secret {
			entry["valueFrom"] = map[string]any{"secretKeyRef": map[string]any{"name": "secret-" + service, "key": key}}
		} else {
			entry["value"] = values[key]
		}
		if i, exists := byName[key]; exists {
			prior, ok := existing[i].(map[string]any)
			if !ok {
				return fmt.Errorf("service %q has an invalid container environment entry", service)
			}
			if source, ok := prior["valueFrom"].(map[string]any); ok {
				if _, isSecret := source["secretKeyRef"]; isSecret && !secret {
					return fmt.Errorf("service %q config key %q conflicts with a rendered secret reference", service, key)
				}
			}
			if _, literal := prior["value"]; literal && secret {
				return fmt.Errorf("service %q secret key %q conflicts with a rendered literal", service, key)
			}
			existing[i] = entry
		} else {
			existing = append(existing, entry)
		}
	}
	container["env"] = existing
	return nil
}

// Check the selected overlay, not just the base we modified: a patch can remove
// the workload or override its configuration after projection.
func validateProjectedConfiguration(root string, service *resources.Service, env *environments.Environment, scope unitScope) error {
	values := map[string]string{}
	if env.ServiceConfig != nil {
		values = env.ServiceConfig.Services[service.Name].Values
	}
	refs := map[string]environments.EnvironmentSecretRemoteRef{}
	if env.ServiceSecrets != nil {
		refs = env.ServiceSecrets.Services[service.Name].RemoteKeys
	}
	identity, err := soleWorkloadIdentity(service.Name, consumedManagedServices(service, env), env)
	if err != nil {
		return err
	}
	expectedSecret, err := expectedServiceSecret(root, scope, service.Name, env)
	if err != nil {
		return err
	}
	if len(values) == 0 && len(refs) == 0 && identity == nil && expectedSecret == nil {
		return nil
	}
	documents, err := effectiveConfiguration(root, env.Name)
	if err != nil {
		return fmt.Errorf("build configuration overlay for %q: %w", service.Name, err)
	}
	if err = validateProjectedSecret(documents, expectedSecret); err != nil {
		return err
	}
	configMaps, err := indexConfigurationMaps(documents)
	if err != nil {
		return err
	}
	accounts := map[namespacedName]map[string]any{}
	for _, doc := range documents {
		if doc.group == "" && doc.kind == "ServiceAccount" {
			key := namespacedName{namespace: metadataString(doc.value, "namespace"), name: metadataString(doc.value, "name")}
			accounts[key] = doc.value
		}
	}
	matched := 0
	workloads := 0
	for _, doc := range documents {
		spec, ok := podSpec(doc)
		if !ok {
			continue
		}
		workloads++
		if err := validateProjectedIdentity(service.Name, doc, accounts, identity); err != nil {
			return err
		}
		if err := validateProjectedSecretNamespace(doc, expectedSecret); err != nil {
			return err
		}
		for _, candidate := range sliceField(spec, "containers") {
			container, ok := candidate.(map[string]any)
			if !ok {
				return fmt.Errorf("service %q has an invalid effective container", service.Name)
			}
			boundService, err := configMaps.service(container, metadataString(doc.value, "namespace"))
			if err != nil {
				return err
			}
			if boundService != service.Name {
				continue
			}
			matched++
			if err := validateContainerConfiguration(container, service.Name, values, refs); err != nil {
				return err
			}
		}
	}
	if (matched == 0 && (len(values) > 0 || len(refs) > 0)) || (workloads == 0 && identity != nil) {
		return fmt.Errorf("service %q declarations bind no effective workload", service.Name)
	}
	return nil
}

func expectedServiceSecret(root string, scope unitScope, service string, env *environments.Environment) (*externalSecret, error) {
	if env.ServiceSecrets == nil {
		return nil, nil
	}
	keys, err := serviceSecretKeys(root, service)
	if err != nil {
		return nil, err
	}
	return serviceSecretProjection(scope, service, env.ServiceSecrets, keys)
}

func effectiveConfiguration(root, environment string) ([]manifest, error) {
	overlay := filepath.Join(root, "overlays", environment)
	built, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), overlay)
	if err != nil {
		return nil, err
	}
	data, err := built.AsYaml()
	if err != nil {
		return nil, err
	}
	documents, _, err := decodeYAML("configuration-overlay.yaml", data)
	return documents, err
}

func validateProjectedSecretNamespace(doc manifest, expected *externalSecret) error {
	if expected == nil || metadataString(doc.value, "namespace") == expected.Metadata.Namespace {
		return nil
	}
	keys := map[string]struct{}{}
	collectSecretKeyRefs(doc.value, expected.Spec.Target.Name, keys)
	if len(keys) > 0 {
		return fmt.Errorf("workload %q references projected secret %q from a different namespace", metadataString(doc.value, "name"), expected.Spec.Target.Name)
	}
	return nil
}

func validateContainerConfiguration(container map[string]any, service string, values map[string]string, refs map[string]environments.EnvironmentSecretRemoteRef) error {
	entries := map[string]map[string]any{}
	for _, item := range sliceField(container, "env") {
		entry, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("service %q has an invalid effective environment entry", service)
		}
		name, _ := entry["name"].(string)
		if _, exists := entries[name]; exists {
			return fmt.Errorf("service %q repeats effective environment key %q", service, name)
		}
		entries[name] = entry
	}
	for key, value := range values {
		entry := entries[key]
		if entry["value"] != value || entry["valueFrom"] != nil {
			return fmt.Errorf("service %q overlay drops or overrides declared configuration key %q", service, key)
		}
	}
	for key := range refs {
		entry := entries[key]
		source, _ := entry["valueFrom"].(map[string]any)
		ref, _ := source["secretKeyRef"].(map[string]any)
		if ref["name"] != "secret-"+service || ref["key"] != key || ref["optional"] == true || entry["value"] != nil {
			return fmt.Errorf("service %q overlay drops or overrides declared secret key %q", service, key)
		}
	}
	return nil
}

func validateProjectedSecret(documents []manifest, expected *externalSecret) error {
	if expected == nil {
		return nil
	}
	data, err := yaml.Marshal(expected.Spec)
	if err != nil {
		return err
	}
	var expectedSpec map[string]any
	if err := yaml.Unmarshal(data, &expectedSpec); err != nil {
		return err
	}
	matched := 0
	for _, doc := range documents {
		if doc.group != "external-secrets.io" || doc.kind != kindExternalSecret || metadataString(doc.value, "namespace") != expected.Metadata.Namespace {
			continue
		}
		spec := mapField(doc.value, "spec")
		name, _ := mapField(spec, "target")["name"].(string)
		if name == "" {
			name = metadataString(doc.value, "name")
		}
		if name != expected.Spec.Target.Name {
			continue
		}
		matched++
		// The complete delivery specification is CLI-owned. Extra templates,
		// dataFrom entries or policies can override an otherwise correct data key.
		if !reflect.DeepEqual(spec, expectedSpec) {
			return fmt.Errorf("overlay overrides projected ExternalSecret %q delivery specification", name)
		}
	}
	if matched != 1 {
		return fmt.Errorf("overlay must contain exactly one projected ExternalSecret for %s/%s, found %d", expected.Metadata.Namespace, expected.Spec.Target.Name, matched)
	}
	return nil
}

func validateProjectedIdentity(service string, doc manifest, accounts map[namespacedName]map[string]any, identity *environments.EnvironmentWorkloadIdentity) error {
	if identity == nil {
		return nil
	}
	template := podTemplate(doc)
	name, _ := mapField(template, "spec")["serviceAccountName"].(string)
	namespace := metadataString(doc.value, "namespace")
	account := accounts[namespacedName{namespace: namespace, name: name}]
	if name == "" || account == nil {
		return fmt.Errorf("service %q overlay drops its declared service account %s/%s", service, namespace, name)
	}
	annotations := mapField(mapField(account, "metadata"), "annotations")
	for key, value := range identity.Annotations {
		if annotations[key] != value {
			return fmt.Errorf("service %q overlay overrides service account annotation %q", service, key)
		}
	}
	labels := mapField(mapField(template, "metadata"), "labels")
	for key, value := range identity.Labels {
		if labels[key] != value {
			return fmt.Errorf("service %q overlay overrides pod label %q", service, key)
		}
	}
	return nil
}
