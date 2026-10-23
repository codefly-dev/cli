package environment

import (
	"bytes"
	"fmt"
	"io"

	"github.com/codefly-dev/cli/pkg/environments"
	"gopkg.in/yaml.v3"
)

// productExclusions reads only the product-owned composition decision. Cloud
// placement and runtime bindings remain in the coordinate producer's contract.
// This deliberately does not interpret the historical full launch intent.
func productExclusions(data []byte, name string) ([]environments.EnvironmentExcludedService, error) {
	var intent struct {
		Environments map[string]struct {
			ExcludeServices *[]environments.EnvironmentExcludedService `yaml:"exclude-services"`
		} `yaml:"environments"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&intent); err != nil {
		return nil, fmt.Errorf("product intent: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("product intent must contain exactly one YAML document")
	}
	for environment, selected := range intent.Environments {
		if selected.ExcludeServices == nil {
			return nil, fmt.Errorf("product intent environment %q requires exclude-services (use [] to clear)", environment)
		}
		env := &environments.Environment{Name: environment, ExcludeServices: *selected.ExcludeServices}
		if err := env.Validate(); err != nil {
			return nil, fmt.Errorf("product intent environment %q: %w", environment, err)
		}
	}
	selected, ok := intent.Environments[name]
	if !ok {
		return nil, fmt.Errorf("product intent does not declare environment %q", name)
	}
	return *selected.ExcludeServices, nil
}
