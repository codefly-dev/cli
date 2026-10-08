package gitops

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

var environmentDefaultKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// withServiceEnvironmentDefaults resolves non-secret service-authored defaults
// below the environment's explicit values and secret references. It copies the
// affected mappings: projecting one service must not change the environment
// subsequently handed to another service or to the module bundle generator.
func withServiceEnvironmentDefaults(root string, service *resources.Service, env *environments.Environment) (*environments.Environment, error) {
	raw, declared := service.Spec["environment-defaults"]
	if !declared {
		return env, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("service %q environment-defaults: %w", service.Name, err)
	}
	var defaults map[string]string
	if err := json.Unmarshal(data, &defaults); err != nil || len(defaults) == 0 {
		return nil, fmt.Errorf("service %q environment-defaults must be a nonempty map of strings", service.Name)
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	for key, value := range defaults {
		if !environmentDefaultKey.MatchString(key) || strings.HasPrefix(key, "CODEFLY__") {
			return nil, fmt.Errorf("service %q environment-defaults key %q is invalid or reserved", service.Name, key)
		}
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("service %q environment-defaults value %q is empty or contains NUL", service.Name, key)
		}
	}
	resolved := *env
	config := environments.EnvironmentServiceConfig{Services: map[string]environments.EnvironmentServiceConfigMapping{}}
	if env.ServiceConfig != nil {
		config.Services = maps.Clone(env.ServiceConfig.Services)
	}
	values := maps.Clone(defaults)
	if env.ServiceSecrets != nil {
		for key := range env.ServiceSecrets.Services[service.Name].RemoteKeys {
			delete(values, key)
		}
	}
	// A reference the service's own render already bound overrides the default
	// too, which is what the configuration contract says and what the
	// environment's own secret references do above. Projecting the default as a
	// literal beside it instead made bindContainerConfiguration refuse the
	// whole render for declaring a fallback the service never needed.
	bound, boundErr := renderedValueFromKeys(root, env.Name, service.Name)
	if boundErr != nil {
		return nil, boundErr
	}
	for key := range bound {
		delete(values, key)
	}
	if env.ServiceConfig != nil {
		maps.Copy(values, env.ServiceConfig.Services[service.Name].Values)
	}
	if len(values) != 0 {
		config.Services[service.Name] = environments.EnvironmentServiceConfigMapping{Values: values}
	}
	if len(config.Services) != 0 {
		resolved.ServiceConfig = &config
	}
	return &resolved, nil
}

// renderedValueFromKeys returns the environment keys the service's own rendered
// containers already bind through a valueFrom — an agent-rendered secretKeyRef,
// typically. Only a service that declares defaults is walked, so a service
// without them pays nothing for this.
func renderedValueFromKeys(root, environment, service string) (map[string]struct{}, error) {
	files, err := configurationSources(root, environment)
	if err != nil {
		return nil, err
	}
	all := make([]manifest, 0, len(files))
	for _, documents := range files {
		all = append(all, documents...)
	}
	configMaps, err := indexConfigurationMaps(all)
	if err != nil {
		return nil, err
	}
	bound := map[string]struct{}{}
	for _, document := range all {
		spec, ok := podSpec(document)
		if !ok {
			continue
		}
		for _, raw := range sliceField(spec, deployContainersField) {
			container := mapFieldOrEmpty(raw)
			if container == nil {
				continue
			}
			identity, identityErr := configMaps.service(container, metadataString(document.value, deployNamespaceField))
			if identityErr != nil {
				return nil, identityErr
			}
			if identity != service {
				continue
			}
			for _, item := range sliceField(container, deployEnvField) {
				entry := mapFieldOrEmpty(item)
				if entry[deployValueFromField] != nil {
					bound[quantityString(entry[envEntryName])] = struct{}{}
				}
			}
		}
	}
	return bound, nil
}
