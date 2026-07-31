package provider

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/provider/configuration"
	"github.com/stretchr/testify/require"
)

func validBinding() *Binding {
	return &Binding{
		Name:           "billing",
		Provider:       "acme/stripe:1.2.3",
		Mode:           ModeManaged,
		DeletionPolicy: DeletionRetain,
		Input:          map[string]string{"account": "acme", "token": "secret://stripe/api-token"},
		Output:         &Output{Contract: configuration.BillingContract},
		Spec:           map[string]any{"region": "us", "nested": map[string]any{"tier": "pro"}},
		Endpoints:      []string{"billing/accounts/rest"},
	}
}

func codes(diagnostics []Diagnostic) []string {
	out := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		out = append(out, diagnostic.Code)
	}
	return out
}

func TestValidateBinding_Valid(t *testing.T) {
	registry := configuration.NewRegistry()
	diagnostics := ValidateBinding(context.Background(), validBinding(), registry)
	require.Empty(t, diagnostics, "a well-formed binding must produce no findings, got %v", codes(diagnostics))
}

func TestValidateBinding_Findings(t *testing.T) {
	registry := configuration.NewRegistry()
	cases := []struct {
		name    string
		mutate  func(*Binding)
		wantOne string
	}{
		{"non-exact identity (no version)", func(b *Binding) { b.Provider = "acme/stripe" }, CodeInvalidIdentity},
		{"latest version rejected", func(b *Binding) { b.Provider = "acme/stripe:latest" }, CodeInvalidIdentity},
		{"no publisher", func(b *Binding) { b.Provider = "stripe:1.0.0" }, CodeInvalidIdentity},
		{"unknown mode", func(b *Binding) { b.Mode = "sideways" }, CodeInvalidMode},
		{"missing mode", func(b *Binding) { b.Mode = "" }, CodeInvalidMode},
		{"unknown deletion policy", func(b *Binding) { b.DeletionPolicy = "nuke" }, CodeInvalidDeletionPolicy},
		{"observe cannot mutate", func(b *Binding) {
			b.Mode = ModeObserve
			b.DeletionPolicy = DeletionDeleteOwned
		}, CodeObserveMutation},
		{"secret in spec", func(b *Binding) {
			b.Spec = map[string]any{"key": "client_secret=totally-fake-test-value"}
		}, CodeSecretInSpec},
		{"reference in spec", func(b *Binding) {
			b.Spec = map[string]any{"key": "secret://stripe/token"}
		}, CodeSecretInSpec},
		{"raw secret in public input", func(b *Binding) {
			b.Input = map[string]string{"token": "client_secret=totally-fake-test-value"}
		}, CodeSecretInPublicInput},
		{"malformed secret reference", func(b *Binding) {
			b.Input = map[string]string{"token": "secret://has spaces/bad"}
		}, CodeInvalidSecretRef},
		{"undeclared output contract", func(b *Binding) {
			b.Output = &Output{Contract: "acme.dev/configuration/unknown@9"}
		}, CodeUndeclaredContract},
		{"literal port endpoint", func(b *Binding) { b.Endpoints = []string{"3000"} }, CodeLiteralEndpoint},
		{"url endpoint", func(b *Binding) { b.Endpoints = []string{"https://api.acme.dev"} }, CodeLiteralEndpoint},
		{"host:port endpoint", func(b *Binding) { b.Endpoints = []string{"localhost:5432"} }, CodeLiteralEndpoint},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binding := validBinding()
			tc.mutate(binding)
			got := codes(ValidateBinding(context.Background(), binding, registry))
			require.Contains(t, got, tc.wantOne, "expected %s among findings, got %v", tc.wantOne, got)
		})
	}
}

func TestValidateBinding_AcceptsSemanticEndpointForms(t *testing.T) {
	registry := configuration.NewRegistry()
	for _, endpoint := range []string{"accounts/rest", "billing/accounts/rest", "billing/accounts/rest::http"} {
		binding := validBinding()
		binding.Endpoints = []string{endpoint}
		got := codes(ValidateBinding(context.Background(), binding, registry))
		require.NotContains(t, got, CodeLiteralEndpoint, "endpoint %q must be accepted", endpoint)
	}
}
