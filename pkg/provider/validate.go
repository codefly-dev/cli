package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/resources"
)

// referenceSchemes are the opaque host namespaces a binding input may use to
// name a secret or captured value. A value using one of these schemes must be a
// canonical opaque reference; any other value is treated as a public literal.
var referenceSchemes = []string{"secret://", "capture://", "opaque://"}

var (
	barePortPattern = regexp.MustCompile(`^\d+$`)
	hostPortPattern = regexp.MustCompile(`:\d+(/|$)`)
)

// ValidateBinding runs the complete offline validation of a single binding and
// returns every finding. An empty slice means the binding is well-formed. This
// never starts an agent, reaches the network, or reads a secret value.
func ValidateBinding(ctx context.Context, binding *Binding, registry *configuration.Registry) []Diagnostic {
	var diagnostics []Diagnostic
	add := func(code, format string, args ...any) {
		diagnostics = append(diagnostics, Diagnostic{Code: code, Binding: binding.Name, Message: fmt.Sprintf(format, args...)})
	}

	// Exact provider identity: publisher/name:version, concrete version.
	if err := validateProviderIdentity(ctx, binding.Provider); err != nil {
		add(CodeInvalidIdentity, "provider identity %q is not an exact publisher/name:version: %v", binding.Provider, err)
	}

	switch binding.Mode {
	case ModeObserve, ModeManaged, ModeDisabled:
	case "":
		add(CodeInvalidMode, "mode is required (one of observe, managed, disabled)")
	default:
		add(CodeInvalidMode, "unknown mode %q (expected observe, managed, or disabled)", binding.Mode)
	}

	switch binding.DeletionPolicy {
	case "", DeletionRetain, DeletionDeleteOwned:
	default:
		add(CodeInvalidDeletionPolicy, "unknown deletion-policy %q (expected retain or delete-owned)", binding.DeletionPolicy)
	}

	// Observe mode cannot express any remote mutation.
	if binding.Mode == ModeObserve && binding.DeletionPolicy == DeletionDeleteOwned {
		add(CodeObserveMutation, "observe mode cannot declare deletion-policy delete-owned")
	}

	// Secrets are forbidden in the public provider spec.
	for _, leaf := range flattenStrings("spec", binding.Spec) {
		if configuration.LooksSecret(leaf.value) || hasReferenceScheme(leaf.value) {
			add(CodeSecretInSpec, "spec value at %s must be public and non-secret", leaf.path)
		}
	}

	// Inputs are public values or opaque secret references — never raw secrets.
	for key, value := range binding.Input {
		if hasReferenceScheme(value) {
			if err := configuration.ValidateOpaqueReference(value); err != nil {
				add(CodeInvalidSecretRef, "input %q is not a canonical opaque reference: %v", key, err)
			}
			continue
		}
		if configuration.LooksSecret(value) {
			add(CodeSecretInPublicInput, "input %q looks like a raw secret; use a secret:// reference instead", key)
		}
	}

	// Bindings cannot target an undeclared output contract.
	if binding.Output != nil && binding.Output.Contract != "" {
		if _, err := registry.Lookup(binding.Output.Contract); err != nil {
			add(CodeUndeclaredContract, "output contract %q is not declared", binding.Output.Contract)
		}
	}

	// Endpoints must be semantic references, never generated ports or URLs.
	for _, endpoint := range binding.Endpoints {
		if err := validateSemanticEndpoint(endpoint); err != nil {
			add(CodeLiteralEndpoint, "endpoint reference %q: %v", endpoint, err)
		}
	}

	return diagnostics
}

func validateProviderIdentity(ctx context.Context, identity string) error {
	if !strings.Contains(identity, "/") || !strings.Contains(identity, ":") {
		return fmt.Errorf("must be of the form publisher/name:version")
	}
	agent, err := resources.ParseAgent(ctx, resources.ProviderAgent, identity)
	if err != nil {
		return err
	}
	if agent.Version == "" || agent.Version == "latest" {
		return fmt.Errorf("a concrete version is required, not %q", agent.Version)
	}
	return nil
}

func validateSemanticEndpoint(reference string) error {
	if strings.Contains(reference, "://") {
		return fmt.Errorf("is a URL, not a semantic endpoint reference")
	}
	if barePortPattern.MatchString(reference) {
		return fmt.Errorf("is a bare port, not a semantic endpoint reference")
	}
	if strings.Contains(reference, "localhost") || hostPortPattern.MatchString(reference) {
		return fmt.Errorf("is a host:port literal, not a semantic endpoint reference")
	}
	if _, err := resources.ParseEndpoint(reference); err != nil {
		return fmt.Errorf("is not a parseable service/endpoint reference")
	}
	return nil
}

func hasReferenceScheme(value string) bool {
	for _, scheme := range referenceSchemes {
		if strings.HasPrefix(value, scheme) {
			return true
		}
	}
	return false
}

type stringLeaf struct {
	path  string
	value string
}

// flattenStrings walks an arbitrary YAML-decoded value and yields every string
// leaf with a dotted path, so secret scanning can reach nested spec values.
func flattenStrings(path string, value any) []stringLeaf {
	switch typed := value.(type) {
	case string:
		return []stringLeaf{{path: path, value: typed}}
	case map[string]any:
		var leaves []stringLeaf
		for key, child := range typed {
			leaves = append(leaves, flattenStrings(path+"."+key, child)...)
		}
		return leaves
	case []any:
		var leaves []stringLeaf
		for index, child := range typed {
			leaves = append(leaves, flattenStrings(fmt.Sprintf("%s[%d]", path, index), child)...)
		}
		return leaves
	default:
		return nil
	}
}
