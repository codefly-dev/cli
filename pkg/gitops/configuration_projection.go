package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	pending := make(map[string][]byte)
	matched := 0
	err := walkRegularFiles(filepath.Join(root, "base"), func(path, relative string, _ os.FileInfo) error {
		extension := strings.ToLower(filepath.Ext(relative))
		if extension != yamlExtension && extension != ymlExtension && extension != jsonExtension {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		documents, customization, err := decodeYAML(relative, data)
		if err != nil {
			return err
		}
		if customization != nil {
			return nil
		}
		changed := false
		for _, document := range documents {
			spec, ok := podSpec(document)
			if !ok {
				continue
			}
			for _, candidate := range sliceField(spec, "containers") {
				container, ok := candidate.(map[string]any)
				if !ok || container["name"] != service {
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
			return nil
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
		return nil
	})
	if err != nil {
		return err
	}
	if matched == 0 {
		return fmt.Errorf("service %q declares configuration but no rendered workload contains its container", service)
	}
	paths := make([]string, 0, len(pending))
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
func validateProjectedConfiguration(root string, service *resources.Service, env *environments.Environment) error {
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
	if len(values) == 0 && len(refs) == 0 && identity == nil {
		return nil
	}
	overlay := filepath.Join(root, "overlays", env.Name)
	built, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), overlay)
	if err != nil {
		return fmt.Errorf("build configuration overlay for %q: %w", service.Name, err)
	}
	data, err := built.AsYaml()
	if err != nil {
		return err
	}
	documents, _, err := decodeYAML("configuration-overlay.yaml", data)
	if err != nil {
		return err
	}
	accounts := map[string]map[string]any{}
	for _, doc := range documents {
		if doc.kind == "ServiceAccount" {
			name, _ := mapField(doc.value, "metadata")["name"].(string)
			accounts[name] = doc.value
		}
	}
	matched := 0
	for _, doc := range documents {
		spec, ok := podSpec(doc)
		if !ok {
			continue
		}
		for _, candidate := range sliceField(spec, "containers") {
			container, ok := candidate.(map[string]any)
			if !ok || container["name"] != service.Name {
				continue
			}
			matched++
			entries := map[string]map[string]any{}
			for _, item := range sliceField(container, "env") {
				entry, ok := item.(map[string]any)
				if !ok {
					return fmt.Errorf("service %q has an invalid effective environment entry", service.Name)
				}
				name, _ := entry["name"].(string)
				if _, exists := entries[name]; exists {
					return fmt.Errorf("service %q repeats effective environment key %q", service.Name, name)
				}
				entries[name] = entry
			}
			for key, value := range values {
				entry := entries[key]
				if entry["value"] != value || entry["valueFrom"] != nil {
					return fmt.Errorf("service %q overlay drops or overrides declared configuration key %q", service.Name, key)
				}
			}
			for key := range refs {
				entry := entries[key]
				source, _ := entry["valueFrom"].(map[string]any)
				ref, _ := source["secretKeyRef"].(map[string]any)
				if ref["name"] != "secret-"+service.Name || ref["key"] != key || ref["optional"] == true || entry["value"] != nil {
					return fmt.Errorf("service %q overlay drops or overrides declared secret key %q", service.Name, key)
				}
			}
			if err := validateProjectedIdentity(service.Name, doc, accounts, identity); err != nil {
				return err
			}
		}
	}
	if matched == 0 {
		return fmt.Errorf("service %q declarations bind no effective workload", service.Name)
	}
	return nil
}

func validateProjectedIdentity(service string, doc manifest, accounts map[string]map[string]any, identity *environments.EnvironmentWorkloadIdentity) error {
	if identity == nil {
		return nil
	}
	template := podTemplate(doc)
	name, _ := mapField(template, "spec")["serviceAccountName"].(string)
	account := accounts[name]
	if name == "" || account == nil {
		return fmt.Errorf("service %q overlay drops its declared service account", service)
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
