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

// PreparedSelection is a consumer's exact prepared keys. Nil preserves the
// unselected consumer's catalog; a non-nil empty selection installs nothing.
type PreparedSelection map[string]bool

func (s PreparedSelection) Includes(key string) bool { return s == nil || s[key] }

// RuntimePreparedSelection projects only the fields owned by
// runtime.installation/v1 (module-runtime/pkg/runnable.Manifest). The runtime
// still validates the complete installation. The CLI has no runtime dependency.
// Only an explicit consumer of runtime-execution installs its releases: public
// root configuration also reaches unrelated services, including tool catalogs.
func RuntimePreparedSelection(service *resources.Service, delivered []*basev0.Configuration) (PreparedSelection, error) {
	if !slices.Contains(service.WorkspaceConfigurationDependencies, RunnableBindingsGroup) || !slices.Contains(service.WorkspaceConfigurationDependencies, RuntimeExecutionGroup) {
		return nil, nil
	}
	keys := PreparedSelection{}
	found := false
	for _, conf := range delivered {
		for _, info := range conf.GetInfos() {
			if info.GetName() != RuntimeExecutionGroup {
				continue
			}
			for _, value := range info.GetConfigurationValues() {
				if !resources.Match(value.GetKey(), "installation-json") {
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
	keys, err := RuntimePreparedSelection(service, delivered)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", service.Name, err)
	}
	return SelectPreparedConfigurations(delivered, keys), nil
}
