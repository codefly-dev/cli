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
