package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeUnresolvedReferenceWorkspace is writeDevWorkspace where api declares
// the `shop` group, and the staging `shop` group names a producer no module of
// the workspace provides and an endpoint worker does not declare.
//
// `shop` lives in the composition root's own `configurations/staging`, so no
// composed module provides it and it reaches EVERY service of the composition —
// api, which declares it, and worker, which does not. That is why the refusal
// below names both consumers: the gate checks the groups a service actually
// receives, not only the ones it declared, because a root group's reference is
// resolved for every service (cli#882).
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

func requireUnresolvedShopReferences(t *testing.T, err error, consumers ...string) {
	t.Helper()
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(err, &unresolved), "want the plan-time refusal, got %v", err)
	faults := make([]string, 0, len(unresolved.References))
	for _, reference := range unresolved.References {
		faults = append(faults, reference.Consumer+" "+reference.Key+" "+reference.Producer)
	}
	slices.Sort(faults)
	// `shop` is a composition-root group, so every consumer the plan covers
	// receives it — whether or not it declared it. Which consumers the plan
	// covers is the caller's business: a module render covers the module's
	// services, a stand-alone dev deploy covers its root alone. Every
	// unresolved reference arrives in one error either way.
	want := make([]string, 0, len(consumers)*2)
	for _, consumer := range consumers {
		want = append(want, consumer+" store-endpoint store/db", consumer+" worker-endpoint shop/worker")
	}
	slices.Sort(want)
	require.Equal(t, want, faults, "every unresolved reference every consumer in the plan receives, in one error")
}

// A module render refuses a configuration error before anything is built or
// pushed: the refusal comes before the registry is prepared (this environment
// declares none, which would otherwise be the error) and before any flow is
// created, so no image build can have been attempted.
func TestRenderModuleRefusesAnUnresolvedReferenceBeforeBuilding(t *testing.T) {
	workspace, module := writeUnresolvedReferenceWorkspace(t)
	_, err := RenderModule(context.Background(), workspace, module, environmentNamed("staging"), "acme-staging", nil)
	requireUnresolvedShopReferences(t, err, "shop/api", "shop/worker")
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
	// Stand-alone: the plan covers the service being deployed and nothing else,
	// so only its own view of the root group is checked.
	requireUnresolvedShopReferences(t, err, "shop/api")
	require.Zero(t, *calls, "no image was built")
}
