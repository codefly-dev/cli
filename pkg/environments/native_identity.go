package environments

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/workloadidentity/native"
	"gopkg.in/yaml.v3"
)

// NativeIdentityCredentials is the JSON audience-to-file mapping delivered only
// to a workload declaring native projections. Paths are owned by the launcher.
const NativeIdentityCredentials = "CODEFLY_NATIVE_IDENTITY_CREDENTIALS"

// NativeIdentityCarrier identifies launcher-owned process values.
func NativeIdentityCarrier(key string) bool {
	return key == NativeIdentityCredentials || key == native.AnchorEnvironment || key == "CODEFLY__WORKLOAD_TOKEN_FILE" || key == "SOLUTION_HOST_LOCAL_TRUST_ANCHOR"
}

// EnvironmentNativeIdentity extends the native run's host identity projection;
// it neither grants application permission nor changes module-manifest delivery.
type EnvironmentNativeIdentity struct {
	Projections []NativeIdentityProjection `yaml:"projections,omitempty"`
	Verifiers   []string                   `yaml:"verifiers,omitempty"`
}

type NativeIdentityProjection struct {
	Service   string   `yaml:"service"`
	Audiences []string `yaml:"audiences"`
}

func (n *EnvironmentNativeIdentity) UnmarshalYAML(node *yaml.Node) error {
	if err := rejectUnknownKeys(node, "native-identity", "projections", "verifiers"); err != nil {
		return err
	}
	type plain EnvironmentNativeIdentity
	return node.Decode((*plain)(n))
}

func (p *NativeIdentityProjection) UnmarshalYAML(node *yaml.Node) error {
	if err := rejectUnknownKeys(node, "native-identity projection", "service", "audiences"); err != nil {
		return err
	}
	type plain NativeIdentityProjection
	return node.Decode((*plain)(p))
}

func (env *Environment) validateNativeIdentity() error {
	n := env.NativeIdentity
	if n == nil {
		return nil
	}
	if env.Host == nil {
		return fmt.Errorf("native-identity requires an environment host declaring the run's trust domain and default audience")
	}
	if env.Cluster != nil || env.Gitops != nil {
		return fmt.Errorf("native-identity is native-only and cannot be combined with cluster or gitops settings")
	}
	if len(n.Projections)+len(n.Verifiers) == 0 {
		return fmt.Errorf("native-identity declares no projections or verifiers")
	}
	if env.ServiceConfig != nil {
		for service, mapping := range env.ServiceConfig.Services {
			for key := range mapping.Values {
				if NativeIdentityCarrier(key) {
					return fmt.Errorf("service-config %s: %s conflicts with native-identity launcher ownership", service, key)
				}
			}
		}
	}
	qualified := func(ref string) error {
		if !gatewayPattern.MatchString(ref) {
			return fmt.Errorf("native-identity service %q must be <module>/<service>", ref)
		}
		return nil
	}
	seen := map[string]bool{}
	for _, p := range n.Projections {
		if err := qualified(p.Service); err != nil {
			return err
		}
		if seen[p.Service] {
			return fmt.Errorf("duplicate native-identity projection service %q", p.Service)
		}
		seen[p.Service] = true
		if len(p.Audiences) == 0 {
			return fmt.Errorf("native-identity projection %q declares no audiences", p.Service)
		}
		audiences := map[string]bool{}
		for _, audience := range p.Audiences {
			if audience == "" || strings.ContainsFunc(audience, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
				return fmt.Errorf("native-identity audience %q must be nonempty without whitespace or control characters", audience)
			}
			if audiences[audience] {
				return fmt.Errorf("duplicate native-identity audience %q for %s", audience, p.Service)
			}
			audiences[audience] = true
		}
	}
	clear(seen)
	for _, ref := range n.Verifiers {
		if err := qualified(ref); err != nil {
			return err
		}
		if seen[ref] {
			return fmt.Errorf("duplicate native-identity verifier %q", ref)
		}
		seen[ref] = true
	}
	return nil
}

// ValidateNativeExecution prevents security declarations from being silently
// discarded by container, free-choice, or deployment execution.
func (env *Environment) ValidateNativeExecution(runtime string) error {
	if env != nil && env.NativeIdentity != nil {
		if err := env.validateNativeIdentity(); err != nil {
			return err
		}
		if native.InPod() {
			return fmt.Errorf("native-identity: %w", native.ErrInPod)
		}
		if runtime != resources.RuntimeContextNative && runtime != resources.RuntimeContextNix {
			return fmt.Errorf("native-identity requires native or nix execution; %q cannot project this run's credentials", runtime)
		}
	}
	return nil
}

// NativeIdentityRecipient reports explicit recipients, using the same qualified
// references validated against the resolved composition at launcher admission.
func (env *Environment) NativeIdentityRecipient(unique string) bool {
	if env == nil || env.NativeIdentity == nil {
		return false
	}
	for _, p := range env.NativeIdentity.Projections {
		if p.Service == unique {
			return true
		}
	}
	for _, verifier := range env.NativeIdentity.Verifiers {
		if verifier == unique {
			return true
		}
	}
	return false
}
