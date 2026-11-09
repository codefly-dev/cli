package runnables

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
)

const RuntimeExecutionGroup = "runtime-execution"

// PreparedSelection compares keys with Runtime configuration lookup semantics.
// An empty selection installs nothing.
type PreparedSelection map[string]bool

func (s PreparedSelection) Includes(key string) bool {
	for selected := range s {
		if resources.Match(selected, key) {
			return true
		}
	}
	return false
}

// PreparedSelectionFor reads the composition's offered tools and Runtime's
// installation references from the actual delivered configuration. Missing
// declarations select nothing. Full installation validation remains Runtime's.
func PreparedSelectionFor(service *resources.Service, delivered []*basev0.Configuration) (PreparedSelection, error) {
	keys, available, offered := PreparedSelection{}, PreparedSelection{}, PreparedSelection{}
	if !slices.Contains(service.WorkspaceConfigurationDependencies, RunnableBindingsGroup) {
		return keys, nil
	}
	identity, identityErr := service.Identity()
	module := ""
	if identityErr == nil {
		module = identity.Module
	}
	found := false
	for _, conf := range delivered {
		for _, info := range conf.GetInfos() {
			for _, value := range info.GetConfigurationValues() {
				switch info.GetName() {
				case RunnableBindingsGroup:
					available[value.GetKey()] = true
				case "runnable-tools":
					if identityErr != nil {
						return nil, identityErr
					}
					if !resources.Match(value.GetKey(), module) {
						continue
					}
					var names []string
					if value.GetSecret() || !strings.HasPrefix(strings.TrimSpace(value.GetValue()), "[") || json.Unmarshal([]byte(value.GetValue()), &names) != nil {
						return nil, fmt.Errorf("runnable-tools/%s must be a public JSON array of prepared ids", module)
					}
					for _, name := range names {
						offered[name] = true
						keys[name] = true
					}
				case RuntimeExecutionGroup:
					if !slices.Contains(service.WorkspaceConfigurationDependencies, RuntimeExecutionGroup) || !resources.Match(value.GetKey(), "installation-json") {
						continue
					}
					if found || value.GetSecret() {
						return nil, fmt.Errorf("runtime-execution/installation-json must be one public installation declaration")
					}
					found = true
					if err := installationKeys(value.GetValue(), keys); err != nil {
						return nil, fmt.Errorf("runtime-execution/installation-json: %w", err)
					}
				}
			}
		}
	}
	for key := range keys {
		if !available.Includes(key) {
			source := "runtime-execution/installation-json"
			if offered.Includes(key) {
				source = "runnable-tools/" + module
			}
			return nil, fmt.Errorf("%s names unknown or unprepared operation %q", source, key)
		}
	}
	return keys, nil
}

func installationKeys(raw string, keys PreparedSelection) error {
	type installation struct {
		Contract string `json:"contract"`
		Bindings []struct {
			Prepared string `json:"prepared"`
		} `json:"bindings"`
	}
	var documents []installation
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		if err := json.Unmarshal([]byte(raw), &documents); err != nil {
			return fmt.Errorf("invalid installation list")
		}
	} else {
		var document installation
		if err := json.Unmarshal([]byte(raw), &document); err != nil {
			return fmt.Errorf("invalid installation object")
		}
		documents = append(documents, document)
	}
	for i, document := range documents {
		if document.Contract != "runtime.installation/v1" {
			return fmt.Errorf("installation %d must declare runtime.installation/v1", i)
		}
		for _, binding := range document.Bindings {
			if binding.Prepared != "" {
				keys[binding.Prepared] = true
			}
		}
	}
	return nil
}

// SelectPreparedConfigurations copies only the filtered group, so resolving
// one service never narrows the manager's catalog for another service.
func SelectPreparedConfigurations(delivered []*basev0.Configuration, keys PreparedSelection) []*basev0.Configuration {
	if keys == nil {
		return delivered
	}
	out := slices.Clone(delivered)
	for i, conf := range out {
		for _, info := range conf.GetInfos() {
			if info.GetName() != RunnableBindingsGroup {
				continue
			}
			out[i] = proto.CloneOf(conf)
			for _, cloned := range out[i].Infos {
				if cloned.GetName() == RunnableBindingsGroup {
					cloned.ConfigurationValues = slices.DeleteFunc(cloned.ConfigurationValues, func(v *basev0.ConfigurationValue) bool { return !keys.Includes(v.GetKey()) })
				}
			}
			break
		}
	}
	return out
}

// PreparedConfigurationsFor applies the installation selection at delivery.
func PreparedConfigurationsFor(service *resources.Service, delivered []*basev0.Configuration) ([]*basev0.Configuration, error) {
	keys, err := PreparedSelectionFor(service, delivered)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", service.Name, err)
	}
	return SelectPreparedConfigurations(delivered, keys), nil
}
