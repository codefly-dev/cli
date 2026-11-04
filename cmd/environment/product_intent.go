package environment

import (
	"bytes"
	"fmt"
	"io"

	"github.com/codefly-dev/cli/pkg/environments"
	"gopkg.in/yaml.v3"
)

// productIntent is what an authored product file decides for one environment:
// its composition exclusions and the product-owned part of its service-secrets
// (environments.ProductSecretPolicy). Cloud placement, runtime bindings and the
// cell's secret store remain in the coordinate producer's contract. This
// deliberately does not interpret the historical full launch intent.
type productIntent struct {
	ExcludeServices []environments.EnvironmentExcludedService
	// ServiceSecrets is nil when the intent declares none for the environment,
	// which leaves the workspace's product-owned service-secrets entries as
	// they are. A key the intent declares EMPTY (generate: [], may-be-empty:
	// []) is withdrawn on import; the decoder keeps an empty list apart from
	// an absent key (an empty, non-nil slice against nil).
	ServiceSecrets *environments.ProductSecretPolicy
}

// withdrawnSecretKeys names, under service-secrets, the product-owned keys the
// intent declares empty: the ones an import withdraws from the workspace.
// Silence is not among them.
func (intent *productIntent) withdrawnSecretKeys() map[string][]string {
	if intent == nil || intent.ServiceSecrets == nil {
		return nil
	}
	var keys []string
	if intent.ServiceSecrets.Generate != nil && len(intent.ServiceSecrets.Generate) == 0 {
		keys = append(keys, "generate")
	}
	if intent.ServiceSecrets.MayBeEmpty != nil && len(intent.ServiceSecrets.MayBeEmpty) == 0 {
		keys = append(keys, "may-be-empty")
	}
	if len(keys) == 0 {
		return nil
	}
	return map[string][]string{"service-secrets": keys}
}

func parseProductIntent(data []byte, name string) (*productIntent, error) {
	var intent struct {
		Environments map[string]struct {
			ExcludeServices *[]environments.EnvironmentExcludedService `yaml:"exclude-services"`
			ServiceSecrets  *environments.ProductSecretPolicy          `yaml:"service-secrets"`
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
		if err := selected.ServiceSecrets.Validate(); err != nil {
			return nil, fmt.Errorf("product intent environment %q: %w", environment, err)
		}
	}
	selected, ok := intent.Environments[name]
	if !ok {
		return nil, fmt.Errorf("product intent does not declare environment %q", name)
	}
	return &productIntent{ExcludeServices: *selected.ExcludeServices, ServiceSecrets: selected.ServiceSecrets}, nil
}
