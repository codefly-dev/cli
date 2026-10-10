package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestRenderModuleBundleDrivesDeployJobsEndToEnd takes deployJobs the whole way
// a promotion does: a real module bundle generator declares them, the bundle
// loader selects the environment, renderModuleBundle projects them, and the
// reconciliation boundary the publish guard compares against is derived from
// what landed on disk. Every other deploy-job test calls renderModuleDeployJobs
// directly, so nothing covered the loadSelectedModuleBundle -> DeployJobs ->
// moduleIncludesUnits chain the Argo Application boundary rests on.
func TestRenderModuleBundleDrivesDeployJobsEndToEnd(t *testing.T) {
	// Every spelling kustomize recognises, through the real bundle: the
	// generator writes its overlay under that name and the projection has to
	// find it, rewrite that same file, and leave a tree kustomize can build.
	for _, spelling := range kustomizationFileNames {
		t.Run(spelling, func(t *testing.T) { renderModuleBundleDeployJobsEndToEnd(t, spelling) })
	}
}

func renderModuleBundleDeployJobsEndToEnd(t *testing.T, spelling string) {
	ctx := context.Background()
	root := t.TempDir()
	moduleDir := filepath.Join(t.TempDir(), "accounts")
	writeDeployJobModuleWorkspace(t, root, moduleDir)
	installDeployJobBundleGenerator(t, spelling)

	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "accounts")
	require.NoError(t, err)
	environment, err := orchestration.SelectEnvironment(workspace, "production")
	require.NoError(t, err)
	require.Equal(t, "accounts", environment.ModuleNamespace(workspace, module.Name))

	// The units are the authoritative source trees the module overlay
	// references, so they sit beside the bundle under one stage.
	stage := t.TempDir()
	env := &environments.Environment{Name: "production", Namespace: "accounts"}
	var graph []InventoryUnit
	for _, service := range []string{"accounts", "store"} {
		writeConsumerTree(t, filepath.Join(stage, "services", service), env.Name, env.Namespace, service, "declared.example")
		graph = append(graph, InventoryUnit{Kind: UnitKindService, Module: "accounts", Name: service,
			Path: "services/" + service, Output: restrictedRenderEvidence()})
	}
	updateDeployTestYAML(t, filepath.Join(stage, "services", "accounts", "base", "deployment.yaml"), func(doc map[string]any) {
		podSpec := mapField(mapField(mapField(doc, "spec"), "template"), "spec")
		podSpec["serviceAccountName"] = "accounts"
	})

	// The render projects each unit's configuration before it renders the
	// module bundle, which is what puts the service's own declared default in
	// the container the deploy job then inherits from.
	accounts, err := module.LoadServiceFromName(ctx, "accounts")
	require.NoError(t, err)
	require.NoError(t, projectServiceConfiguration(ctx, filepath.Join(stage, "services", "accounts"),
		accounts, env, scopeOf(env), serviceInjection{}))

	destination := filepath.Join(stage, moduleBundleDir)
	require.NoError(t, renderModuleBundle(ctx, workspace, module, environment, destination, graph))

	// The generator declared one deploy job, so the bundle must have produced
	// its documents inside the module's own named overlay.
	documents := buildOverlay(t, destination, "production")
	importJob := deployObject(t, documents, kindJob, "role-catalog-import")
	container := sliceField(mustPodSpec(t, importJob), "containers")[0].(map[string]any)
	require.Equal(t, []any{"role-catalog-import", "-catalog", "/codefly/deploy-catalog/catalog.json"}, container["args"])
	require.Contains(t, container["image"], "@sha256:")

	// And the boundary the publish guard refuses a change of is read back off
	// the tree the bundle wrote, not asserted by the caller.
	require.True(t, moduleIncludesUnits(destination, "production"),
		"a bundle declaring deployJobs must report an aggregate reconciliation boundary")
	require.NoError(t, validateModuleDeployJobBoundary(stage,
		&RenderOptions{ModulePath: moduleBundleDir, Environment: "production", ModuleIncludesUnits: true}))
	require.Error(t, validateModuleDeployJobBoundary(stage,
		&RenderOptions{ModulePath: moduleBundleDir, Environment: "production"}),
		"the inventory must not be able to claim separate Applications once deploy jobs are rendered")

	// The aggregate's whole point is that the module overlay reads the unit
	// overlays, which is only a resolvable reference from the owned tree. That
	// is why the bundle's own validation runs before the deploy jobs are
	// projected and the references are checked over the owned tree instead.
	rewritten, data, err := readDeployKustomization(filepath.Join(destination, "overlays", "production"), "accounts", "production")
	require.NoError(t, err)
	require.Equal(t, spelling, filepath.Base(rewritten), "the projection must rewrite the file it read, not a new one")
	var customization map[string]any
	require.NoError(t, yaml.Unmarshal(data, &customization))
	require.Contains(t, sliceField(customization, resourcesKey), "../../../services/accounts/overlays/production")
	_, ownedErr := validateKustomization(filepath.ToSlash(filepath.Join(moduleBundleDir, "overlays", "production", spelling)), customization)
	require.NoError(t, ownedErr, "owned-tree-relative, the unit references resolve")
	_, bundleErr := validateKustomization(filepath.ToSlash(filepath.Join("overlays", "production", spelling)), customization)
	require.ErrorContains(t, bundleErr, "escapes the owned tree",
		"bundle-relative they do not, so renderModuleDeployJobs must run after validateTransportNeutralModuleBundle")
}

func mustPodSpec(t *testing.T, document manifest) map[string]any {
	t.Helper()
	spec, ok := podSpec(document)
	require.True(t, ok, "%s carries no pod spec", document.kind)
	return spec
}

// writeDeployJobModuleWorkspace lays down a single-module workspace whose
// "accounts" service declares a runtime dependency on "store" and a non-secret
// environment default, plus the committed catalog a deploy job mounts.
func writeDeployJobModuleWorkspace(t *testing.T, root, moduleDir string) {
	t.Helper()
	files := map[string]string{
		filepath.Join(moduleDir, resources.ModuleConfigurationName): `kind: module
name: accounts
agent:
  kind: codefly:module
  publisher: codefly.dev
  name: gitops-test
  version: 1.0.0
services:
  - name: accounts
  - name: store
`,
		filepath.Join(moduleDir, "services", "accounts", resources.ServiceConfigurationName): `kind: service
name: accounts
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
spec:
  environment-defaults:
    AUDIT_SINK: postgres
service-dependencies:
  - name: store
    endpoints:
      - name: tcp
`,
		filepath.Join(moduleDir, "services", "store", resources.ServiceConfigurationName): `kind: service
name: store
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
`,
		filepath.Join(moduleDir, "deployment", "generated", "roles.json"): `{"roles":[{"name":"reader"}]}`,
		filepath.Join(root, resources.WorkspaceConfigurationName): `name: workspace
layout: modules
modules:
  - name: accounts
    path: ` + moduleDir + `
environments:
  - name: production
    namespace: accounts
    cluster:
      kind: k3d
`,
	}
	for path, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

// installDeployJobBundleGenerator installs a codefly:module agent whose bundle
// declares a required deploy job for the accounts service.
func installDeployJobBundleGenerator(t *testing.T, spelling string) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	agent := &resources.Agent{Kind: resources.ModuleAgent, Publisher: "codefly.dev", Name: "gitops-test", Version: "1.0.0"}
	binary, err := agent.Path(context.Background())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(binary), 0o755))
	generator := `#!/bin/sh
set -eu
module_dir="$1"
destination="$module_dir/deployment/kustomize"
mkdir -p "$destination/overlays/production"
cat > "$destination/overlays/production/` + spelling + `" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
EOF
cat > "$destination/bundle.json" <<'EOF'
{
  "schemaVersion": "codefly.dev/module-bundle/v1",
  "module": "accounts",
  "environments": [{
    "name": "production",
    "namespace": "accounts",
    "cluster": "k3d",
    "resourcePath": "overlays/production",
    "services": ["accounts", "store"],
    "deployJobs": [{
      "name": "role-catalog-import",
      "service": "accounts",
      "command": "role-catalog-import",
      "catalog": "deployment/generated/roles.json",
      "writes": {"service": "store", "endpoint": "tcp", "port": 5432},
      "after": ["store"],
      "serviceEnvironment": ["AUDIT_SINK"]
    }]
  }]
}
EOF
`
	require.NoError(t, os.WriteFile(binary, []byte(generator), 0o755))
}
