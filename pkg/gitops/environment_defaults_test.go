package gitops

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestServiceEnvironmentDefaultsReachEffectiveWorkload(t *testing.T) {
	for _, explicit := range []string{"", "bigquery"} {
		t.Run("override="+explicit, func(t *testing.T) {
			env := &environments.Environment{Name: "production", Namespace: "accounts"}
			if explicit != "" {
				env.ServiceConfig = &environments.EnvironmentServiceConfig{Services: map[string]environments.EnvironmentServiceConfigMapping{
					"accounts": {Values: map[string]string{"AUDIT_SINK": explicit}},
				}}
			}
			service := &resources.Service{Name: "accounts", Spec: map[string]any{"environment-defaults": map[string]any{
				"AUDIT_SINK": "postgres", "NOTICE": "  retain whitespace\n",
			}}}
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, service.Name, "declared.example")
			require.NoError(t, projectServiceConfiguration(t.Context(), root, service, env, scopeOf(env), serviceInjection{}))
			want := explicit
			if want == "" {
				want = "postgres"
			}
			values := containerEnvironment(t, buildOverlay(t, root, env.Name))
			require.Equal(t, want, values["AUDIT_SINK"]["value"])
			require.Equal(t, "  retain whitespace\n", values["NOTICE"]["value"])
			if explicit == "" {
				require.Nil(t, env.ServiceConfig, "projection must not mutate the imported coordinate")
			} else {
				require.Equal(t, explicit, env.ServiceConfig.Services[service.Name].Values["AUDIT_SINK"])
				require.NotContains(t, env.ServiceConfig.Services[service.Name].Values, "NOTICE")
			}
		})
	}
}

func TestServiceEnvironmentDefaultYieldsToExplicitSecret(t *testing.T) {
	env := injectionContract(t)
	service := &resources.Service{Name: "api", Spec: map[string]any{"environment-defaults": map[string]any{"DATABASE_PASSWORD": "development-only"}}}
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, service.Name, "declared.example")
	require.NoError(t, projectServiceConfiguration(t.Context(), root, service, env, scopeOf(env), serviceInjection{}))
	value := containerEnvironment(t, buildOverlay(t, root, env.Name))["DATABASE_PASSWORD"]
	require.NotContains(t, value, "value")
	require.Contains(t, value, "valueFrom")
}

func TestServiceEnvironmentDefaultsRejectInvalidDeclarations(t *testing.T) {
	for _, value := range []any{nil, "postgres", map[string]any{}, map[string]any{"AUDIT_SINK": 1}, map[string]any{"AUDIT_SINK": " "}, map[string]any{"CODEFLY__SERVICE": "other"}, map[string]any{"BAD=KEY": "value"}} {
		_, err := withServiceEnvironmentDefaults(&resources.Service{Name: "accounts", Spec: map[string]any{"environment-defaults": value}}, &environments.Environment{Name: "production"})
		require.Error(t, err)
	}
}
