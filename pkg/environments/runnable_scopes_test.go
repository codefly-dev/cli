package environments_test

import (
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

const scopeSelectionWorkspace = `name: product
environments:
  - name: staging
    runnable-scope-selections:
      SAAS__INVOKE_SOURCE: &source-scopes
        - slot: source
          invoke:
            - resource_kind: datasource.sources
              actions: [invoke, read]
              resource_ids: ["00123"]
          lookup:
            - resource_kind: datasource.sources
              actions: [read]
              resource_ids: ["00123"]
      SAAS__SECOND_BINDING: *source-scopes
`

func TestScopeSelectionsUseTheExistingEnvironmentRoundTrip(t *testing.T) {
	var workspace resources.Workspace
	require.NoError(t, yaml.Unmarshal([]byte(scopeSelectionWorkspace), &workspace))
	env, err := environments.Select(&workspace, "staging")
	require.NoError(t, err)
	selected := env.RunnableScopeSelections["SAAS__INVOKE_SOURCE"][0]
	require.Equal(t, "source", selected.Slot)
	require.Equal(t, []string{"00123"}, selected.Invoke[0].ResourceIds)
	require.True(t, proto.Equal(selected, env.RunnableScopeSelections["SAAS__SECOND_BINDING"][0]))
	require.Empty(t, env.Runtime().Extensions, "installation input is not agent runtime configuration")
	resource, err := env.Resource()
	require.NoError(t, err)
	reloaded, err := environments.FromRuntime(resource)
	require.NoError(t, err)
	require.True(t, proto.Equal(selected, reloaded.RunnableScopeSelections["SAAS__INVOKE_SOURCE"][0]))
	selected.Invoke[0].ResourceIds[0] = "changed"
	again, err := environments.Select(&workspace, "staging")
	require.NoError(t, err)
	require.Equal(t, []string{"00123"}, again.RunnableScopeSelections["SAAS__INVOKE_SOURCE"][0].Invoke[0].ResourceIds)
}

func TestScopeSelectionYAMLRefusesUnknownCoreFields(t *testing.T) {
	for _, field := range []string{"slot", "resource_ids", "actions"} {
		t.Run(field, func(t *testing.T) {
			var workspace resources.Workspace
			require.NoError(t, yaml.Unmarshal([]byte(strings.ReplaceAll(scopeSelectionWorkspace, field+":", field+"_typo:")), &workspace))
			_, err := environments.Select(&workspace, "staging")
			require.ErrorContains(t, err, "unknown field")
			require.ErrorContains(t, err, field+"_typo")
		})
	}
}

const declinedSelectionWorkspace = `name: product
environments:
  - name: base
    runnable-scope-selections:
      SAAS__INVOKE_SOURCE: []
      SAAS__SECOND_BINDING:
        - slot: source
          invoke:
            - resource_kind: datasource.sources
              actions: [invoke, read]
              resource_ids: ["00123"]
`

// An explicitly empty list is a decline, distinct from an absent key, and it
// must survive every way the environment is read and written: YAML through
// Core's workspace load (which keeps the CLI block as an extension), back out
// through Resource, and in again through FromRuntime. A decoder that collapsed
// [] to nil would turn a decline into an omission, which generation refuses.
func TestEmptyScopeSelectionListDeclinesAndSurvivesTheRoundTrip(t *testing.T) {
	var workspace resources.Workspace
	require.NoError(t, yaml.Unmarshal([]byte(declinedSelectionWorkspace), &workspace))
	env, err := environments.Select(&workspace, "base")
	require.NoError(t, err)
	declined, present := env.RunnableScopeSelections["SAAS__INVOKE_SOURCE"]
	require.True(t, present)
	require.NotNil(t, declined, "[] decodes to an empty list, never to nil")
	require.Empty(t, declined)
	require.True(t, env.DeclinesRunnableBinding("SAAS__INVOKE_SOURCE"))
	require.False(t, env.DeclinesRunnableBinding("SAAS__SECOND_BINDING"), "a list of selections is not a decline")
	require.False(t, env.DeclinesRunnableBinding("SAAS__ABSENT"), "an absent key is not a decline")
	require.Equal(t, []string{"SAAS__INVOKE_SOURCE"}, env.DeclinedRunnableBindings())

	resource, err := env.Resource()
	require.NoError(t, err)
	reloaded, err := environments.FromRuntime(resource)
	require.NoError(t, err)
	require.Equal(t, []string{"SAAS__INVOKE_SOURCE"}, reloaded.DeclinedRunnableBindings())
	written, err := yaml.Marshal(reloaded)
	require.NoError(t, err)
	require.Contains(t, string(written), "SAAS__INVOKE_SOURCE: []")
}

// A null value reads as neither: it is refused when the environment is
// selected, and naming both spellings tells the author which one they meant.
func TestNullScopeSelectionIsRefused(t *testing.T) {
	for _, spelled := range []string{"SAAS__INVOKE_SOURCE:\n", "SAAS__INVOKE_SOURCE: ~\n", "SAAS__INVOKE_SOURCE: null\n"} {
		t.Run(strings.TrimSpace(spelled), func(t *testing.T) {
			var workspace resources.Workspace
			document := strings.Replace(declinedSelectionWorkspace, "SAAS__INVOKE_SOURCE: []\n", spelled, 1)
			require.NoError(t, yaml.Unmarshal([]byte(document), &workspace))
			_, err := environments.Select(&workspace, "base")
			require.ErrorContains(t, err, "runnable-scope-selections[SAAS__INVOKE_SOURCE] is null")
			require.ErrorContains(t, err, "write [] to decline")
		})
	}
}
