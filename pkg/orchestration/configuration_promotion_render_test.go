package orchestration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Exercise promotion and the same owned-tree guard used by pkg/gitops's render
// tests together: an accepted request alone does not prove its ConfigMap is
// safe, because the guard classifies the delivered carrier, including its group.
func TestPromotedConfigurationPassesRenderGuard(t *testing.T) {
	workspace := &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name:                "datasource-oauth",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "providers", Value: "{}"}},
		}},
	}
	service := &basev0.Configuration{
		Origin: "saas/auth-gateway",
		Infos: []*basev0.ConfigurationInformation{{
			Name:                "identity",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "audience", Value: "api"}},
		}},
	}
	safe, dependencies, references, err := orchestration.PromotableDeploymentConfigurationsForTest(service, []*basev0.Configuration{workspace}, "secret-auth-gateway")
	require.NoError(t, err)

	// Encode what remains inline under core's carrier names, and deliver the
	// extracted references as Kubernetes env entries, as a builder would.
	data := map[string]any{}
	for _, configuration := range append([]*basev0.Configuration{safe}, dependencies...) {
		variables, err := resources.ConfigurationAsEnvironmentVariables(configuration, "production", false)
		require.NoError(t, err)
		for _, variable := range variables {
			data[variable.Key] = variable.Value
		}
	}
	var environment []map[string]any
	for key, reference := range references {
		environment = append(environment, map[string]any{
			"name": key,
			"valueFrom": map[string]any{"secretKeyRef": map[string]any{
				"name": reference.GetName(), "key": reference.GetKey(), "optional": reference.GetOptional(),
			}},
		})
	}
	manifests := []map[string]any{
		{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "configuration"}, "data": data,
		},
		{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "auth-gateway"},
			"spec": map[string]any{
				"selector": map[string]any{"matchLabels": map[string]any{"app": "auth-gateway"}},
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "auth-gateway"}},
					"spec": map[string]any{"containers": []map[string]any{{
						"name": "auth-gateway", "image": "registry.example.com/saas/api@sha256:" + strings.Repeat("a", 64),
						"env":     environment,
						"envFrom": []map[string]any{{"configMapRef": map[string]any{"name": "configuration"}}},
					}}},
				},
			},
		},
	}
	result, err := gitops.RenderOwnedTree(t.Context(), &gitops.RenderOptions{
		Destination: filepath.Join(t.TempDir(), "owned"), Module: "saas", Environment: "production", Promotable: true,
	}, func(_ context.Context, root string) error {
		var rendered strings.Builder
		encoder := yaml.NewEncoder(&rendered)
		for _, manifest := range manifests {
			if err := encoder.Encode(manifest); err != nil {
				return err
			}
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "manifests.yaml"), []byte(rendered.String()), 0o644)
	})
	require.NoError(t, err, "promotion must remove the ConfigMap value the render guard refuses")
	require.NoError(t, gitops.ValidateRenderedTree(result.Path, "", true))
	require.Equal(t, map[string]any{
		"CODEFLY__SERVICE_CONFIGURATION__SAAS__AUTH_GATEWAY__IDENTITY__AUDIENCE": "api",
	}, data)
	const secretKey = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__DATASOURCE_OAUTH__PROVIDERS"
	require.Equal(t, []map[string]any{{
		"name": secretKey,
		"valueFrom": map[string]any{"secretKeyRef": map[string]any{
			"name": "secret-auth-gateway", "key": secretKey, "optional": false,
		}},
	}}, environment)
}
