package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// installAccountAgents stands in for the service agents with ones that each
// render a service account of their own, named by accountFor. Every service then
// binds an account its own unit defines, which the per-service checks are right
// to admit — so what is left is the question only a whole render can answer, and
// this is the shape that asks it.
func installAccountAgents(t *testing.T, accountFor func(service string) string) {
	t.Helper()
	previous := serviceFlow
	t.Cleanup(func() { serviceFlow = previous })
	serviceFlow = func(
		ctx context.Context,
		workspace *resources.Workspace,
		module *resources.Module,
		root *resources.Service,
		env *environments.Environment,
		_ renderBuild,
		_ orchestration.OutputSink,
		destination func(*resources.Module, *resources.Service) string,
		record func(map[string]*builderv0.DeploymentOutput),
		recordServices func(map[string]*resources.Service),
		recordSelfEndpoints func(map[string]map[string]string),
		recordConfigurations func(map[string]*basev0.Configuration, map[string][]string),
		recordInClusterPorts func(map[string]map[string]uint32),
	) error {
		graph := []*resources.Service{root}
		for _, dependency := range root.ServiceDependencies {
			loaded, loadErr := module.LoadServiceFromName(ctx, dependency.Name)
			if loadErr != nil {
				return loadErr
			}
			graph = append(graph, loaded)
		}
		outputs := map[string]*builderv0.DeploymentOutput{}
		services := map[string]*resources.Service{}
		for _, service := range graph {
			writeAccountTree(t, destination(module, service), env.Name, service.Name, accountFor(service.Name))
			unique := resources.ServiceUnique(module.Name, service.Name)
			outputs[unique] = fakeRestrictedOutput()
			services[unique] = service
		}
		if record != nil {
			record(outputs)
		}
		if recordServices != nil {
			recordServices(services)
		}
		if recordSelfEndpoints != nil {
			recordSelfEndpoints(map[string]map[string]string{})
		}
		if recordConfigurations != nil {
			recordConfigurations(map[string]*basev0.Configuration{}, map[string][]string{})
		}
		if recordInClusterPorts != nil {
			recordInClusterPorts(map[string]map[string]uint32{})
		}
		return nil
	}
}

// writeAccountTree is one service unit that defines an account and binds its
// workload to it.
func writeAccountTree(t *testing.T, root, environment, service, account string) {
	t.Helper()
	base := filepath.Join(root, "base")
	overlay := filepath.Join(root, "overlays", environment)
	require.NoError(t, os.MkdirAll(base, 0o755))
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	files := map[string]string{
		filepath.Join(base, "serviceaccount.yaml"): fmt.Sprintf(
			"apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: %s\n  namespace: acme\n  labels:\n    app.kubernetes.io/managed-by: codefly\n", account),
		filepath.Join(base, "deployment.yaml"): fmt.Sprintf(
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %[1]s\n  namespace: acme\nspec:\n  template:\n    metadata:\n      labels:\n        app: %[1]s\n    spec:\n      serviceAccountName: %[2]s\n      containers:\n        - name: %[1]s\n          image: registry.example.com/acme/%[1]s@sha256:0000000000000000000000000000000000000000000000000000000000000000\n",
			service, account),
		filepath.Join(base, "kustomization.yaml"):    "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - serviceaccount.yaml\n  - deployment.yaml\n",
		filepath.Join(overlay, "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n",
	}
	for path, content := range files {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

// A principal belongs to one service, and a single-module render has to say so.
// Each service here binds an account its own unit defines, so every per-service
// check admits it; only something that sees the whole render can tell that two of
// them are the same principal.
func TestAModuleRenderRefusesTwoServicesOnOnePrincipal(t *testing.T) {
	installAccountAgents(t, func(string) string { return "shared" })
	workspace, module, env := containerPortsFixture(t, nil)

	_, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), `both bind the service account "shared"`)
	require.Contains(t, err.Error(), "shop/api")
	require.Contains(t, err.Error(), "shop/store")

	_, statErr := os.Stat(moduleRenderDestination(workspace, "shop"))
	require.True(t, os.IsNotExist(statErr), "a refused render installs nothing")
}

// The same render with each service on its own account is admitted, so the
// refusal above is about the collision and not about the shape of the fixture.
func TestAModuleRenderAdmitsDistinctPrincipals(t *testing.T) {
	installAccountAgents(t, func(service string) string { return service })
	workspace, module, env := containerPortsFixture(t, nil)

	_, err := RenderModule(context.Background(), workspace, module, env, "acme-staging", nil)
	require.NoError(t, err)
}
