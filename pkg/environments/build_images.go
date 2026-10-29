package environments

import (
	"fmt"
	"strings"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// BuildImages selects agent-owned image roles without changing agent recipes.
// Registry relocation keeps the digest; type and service entries can select
// different content. An explicit service entry wins for the same role.
type BuildImages struct {
	Registries map[string]string                `yaml:"registries,omitempty" json:"registries,omitempty"`
	Types      map[string]map[string]BuildImage `yaml:"types,omitempty" json:"types,omitempty"`
	Services   map[string]map[string]BuildImage `yaml:"services,omitempty" json:"services,omitempty"`
}

type BuildImage struct {
	Reference string `yaml:"reference" json:"reference"`
	Variant   string `yaml:"variant" json:"variant"`
}

func (policy *BuildImages) Validate() error {
	if policy == nil {
		return nil
	}
	// Registry relocations use the existing canonical registry/path grammar.
	if err := (&EnvironmentRegistry{Mirrors: policy.Registries}).Validate(); err != nil {
		return fmt.Errorf("build-images registries: %w", err)
	}
	for kind, selections := range map[string]map[string]map[string]BuildImage{"types": policy.Types, "services": policy.Services} {
		for selector, roles := range selections {
			parts := strings.Split(selector, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(selector, " \t\r\n:*@") || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
				return fmt.Errorf("build-images %s selector %q must be an exact %s", kind, selector, map[string]string{"types": "publisher/agent", "services": "module/service"}[kind])
			}
			if len(roles) == 0 {
				return fmt.Errorf("build-images %s %q declares no roles", kind, selector)
			}
			for role, input := range roles {
				if err := services.ValidateBuildImageInput(&builderv0.BuildImageInput{Role: role, Reference: input.Reference, Variant: input.Variant}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (policy *BuildImages) Applies(agent, service string) bool {
	return policy != nil && (len(policy.Registries) > 0 || len(policy.Types[agent]) > 0 || len(policy.Services[service]) > 0)
}

// Resolve preserves declaration order and leaves the caller's values untouched.
// Explicit bindings are destination references and are never relocated again.
func (policy *BuildImages) Resolve(agent, service string, declarations []*builderv0.BuildImageInput) ([]*builderv0.BuildImageInput, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	defaults, err := services.ResolveBuildImages(declarations, nil)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return defaults, nil
	}
	chosen := make(map[string]BuildImage)
	for k, v := range policy.Types[agent] {
		chosen[k] = v
	}
	for k, v := range policy.Services[service] {
		chosen[k] = v
	}
	bindings := make([]*builderv0.BuildImageInput, 0, len(chosen))
	for role, input := range chosen {
		bindings = append(bindings, &builderv0.BuildImageInput{Role: role, Reference: input.Reference, Variant: input.Variant})
	}
	resolved, err := services.ResolveBuildImages(defaults, bindings)
	if err != nil {
		return nil, err
	}
	for _, input := range resolved {
		if _, explicit := chosen[input.Role]; !explicit && input.Reference != "scratch" {
			input.Reference = MirroredReference(input.Reference, policy.Registries)
		}
	}
	return services.ResolveBuildImages(defaults, resolved)
}
