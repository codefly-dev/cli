package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeUnresolvedReferenceWorkspace is writeDevWorkspace where api declares
// the `shop` group, and the staging `shop` group names a producer no module of
// the workspace provides and an endpoint worker does not declare.
func writeUnresolvedReferenceWorkspace(t *testing.T) (*resources.Workspace, *resources.Module) {
	t.Helper()
	workspace, _ := writeDevWorkspace(t)
	api := devServiceYAML("api") + "workspace-configuration-dependencies:\n  - shop\n"
	for rel, content := range map[string]string{
		filepath.Join("modules", "shop", "services", "api", resources.ServiceConfigurationName): api,
		filepath.Join("configurations", "staging", "shop.env"): "store-endpoint=${endpoint:store/db/tcp}\n" +
			"worker-endpoint=${endpoint:shop/worker/grpc}\n",
	} {
		full := filepath.Join(workspace.Dir(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	return workspace, module
}

func requireUnresolvedShopReferences(t *testing.T, err error) {
	t.Helper()
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(err, &unresolved), "want the plan-time refusal, got %v", err)
	var keys, producers []string
	for _, reference := range unresolved.References {
		require.Equal(t, "shop/api", reference.Consumer)
		keys = append(keys, reference.Key)
		producers = append(producers, reference.Producer)
	}
	require.Equal(t, []string{"store-endpoint", "worker-endpoint"}, keys, "every unresolved reference, in one error")
	require.Equal(t, []string{"store/db", "shop/worker"}, producers)
}

// A module render refuses a configuration error before anything is built or
// pushed: the refusal comes before the registry is prepared (this environment
// declares none, which would otherwise be the error) and before any flow is
// created, so no image build can have been attempted.
func TestRenderModuleRefusesAnUnresolvedReferenceBeforeBuilding(t *testing.T) {
	workspace, module := writeUnresolvedReferenceWorkspace(t)
	_, err := RenderModule(context.Background(), workspace, module, environmentNamed("staging"), "acme-staging", nil)
	requireUnresolvedShopReferences(t, err)
}

// A dev deploy refuses a configuration error of the service it deploys before
// building its image.
func TestDeployDevRefusesAnUnresolvedReferenceBeforeBuilding(t *testing.T) {
	workspace, module := writeUnresolvedReferenceWorkspace(t)
	renderDevFixture(t, workspace)
	calls := stubBuild(t, []string{"registry.example.com/acme/api:0.0.1@" + devNewDigest}, nil)

	source := filepath.Join(workspace.Dir(), "modules", "shop", "services", "api")
	_, err := DeployDev(context.Background(), &DevRequest{
		Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"),
		AppProject: "acme-staging", Source: DevSource{Dir: source, Origin: DevSourceOverride},
	})
	requireUnresolvedShopReferences(t, err)
	require.Zero(t, *calls, "no image was built")
}
