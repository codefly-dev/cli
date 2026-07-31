package provider

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeBindings(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, BindingsFileName), []byte(content), 0o600))
	return dir
}

const validDocument = `
schema: codefly.provider-bindings/v0
environments:
  local-dogfood:
    billing:
      provider: acme/stripe:1.2.3
      mode: managed
    email:
      provider: acme/resend:2.0.0
      mode: observe
`

func TestLoadDocument_MissingIsNotAnError(t *testing.T) {
	doc, present, err := LoadDocument(t.TempDir())
	require.NoError(t, err)
	require.False(t, present)
	require.Nil(t, doc)
}

func TestLoadDocument_PopulatesKeysAndSorts(t *testing.T) {
	doc, present, err := LoadDocument(writeBindings(t, validDocument))
	require.NoError(t, err)
	require.True(t, present)

	bindings := doc.ForEnvironment("local-dogfood")
	require.Len(t, bindings, 2)
	require.Equal(t, "billing", bindings[0].Name, "bindings must be sorted by name")
	require.Equal(t, "email", bindings[1].Name)
	require.Equal(t, "local-dogfood", bindings[0].Environment)

	billing, ok := doc.Binding("local-dogfood", "billing")
	require.True(t, ok)
	require.Equal(t, "acme/stripe:1.2.3", billing.Provider)

	_, ok = doc.Binding("local-dogfood", "absent")
	require.False(t, ok)

	require.Empty(t, doc.ForEnvironment("production"))
}

func TestLoadDocument_UnknownSchema(t *testing.T) {
	_, present, err := LoadDocument(writeBindings(t, "schema: codefly.provider-bindings/v99\nenvironments: {}\n"))
	require.True(t, present)
	require.ErrorIs(t, err, ErrUnknownSchema)
}

func TestLoadDocument_UnknownFieldRejected(t *testing.T) {
	_, _, err := LoadDocument(writeBindings(t, "schema: codefly.provider-bindings/v0\nsurprise: true\n"))
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrUnknownSchema))
}

func TestBindingView_RedactsInputValues(t *testing.T) {
	binding := &Binding{
		Name:      "billing",
		Provider:  "acme/stripe:1.2.3",
		Mode:      ModeManaged,
		Input:     map[string]string{"token": "secret://stripe/api-token", "account": "acme-live-account"},
		Endpoints: []string{"billing/accounts/rest"},
	}
	view := binding.View(true)

	require.Equal(t, DeletionRetain, DeletionPolicy(view.DeletionPolicy), "unset deletion policy defaults to retain")
	require.ElementsMatch(t, []string{"account", "token"}, view.InputKeys)
	require.NotContains(t, view.InputKeys, "secret://stripe/api-token")
	require.NotContains(t, view.InputKeys, "acme-live-account")
}

// The view must be an independent copy: mutating the source binding after
// building a view must not change the already-produced view.
func TestBindingView_EndpointsAreCopied(t *testing.T) {
	binding := &Binding{
		Name:      "billing",
		Provider:  "acme/stripe:1.2.3",
		Mode:      ModeManaged,
		Endpoints: []string{"billing/accounts/rest"},
	}
	view := binding.View(true)
	binding.Endpoints[0] = "mutated/after/view"
	require.Equal(t, "billing/accounts/rest", view.Endpoints[0], "the view must not alias the binding's slice")
}
