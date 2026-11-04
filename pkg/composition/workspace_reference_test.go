package composition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/modulecache"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const importedPlatformCommit = "5a8d5ffa0d1e2c3b4a5968778695a4b3c2d1e0f9"

// importingWorkspace writes a product workspace importing platform-core by
// source and commit pin, with platform-core already in the module cache at
// that pin — what a previous pull, or a module pin on the same source, leaves
// there. apps/web requires an interface platform/api implements.
func importingWorkspace(t *testing.T) string {
	t.Helper()
	cacheRoot := t.TempDir()
	t.Setenv(modulecache.Env, cacheRoot)
	core := filepath.Join(cacheRoot, "obin-ai", "platform-core", importedPlatformCommit)
	product := t.TempDir()
	agent := "agent:\n    kind: codefly:service\n    name: go\n    version: 0.0.67\n    publisher: codefly.dev\n"
	files := map[string]string{
		filepath.Join(core, "workspace.codefly.yaml"):                             "name: platform-core\nlayout: modules\nmodules:\n    - name: platform\n",
		filepath.Join(core, "modules/platform/module.codefly.yaml"):               "kind: module\nname: platform\nservices:\n    - name: api\ninterface:\n    endpoints:\n        - service: api\n          endpoint: grpc\n          visibility: public\n          implements:\n              - example.dev/widgets@1.2.0\n",
		filepath.Join(core, "modules/platform/services/api/service.codefly.yaml"): "kind: service\nname: api\nversion: 0.0.0\n" + agent + "endpoints:\n    - name: grpc\n      api: grpc\n",
		filepath.Join(product, "workspace.codefly.yaml"):                          "name: product\nlayout: modules\nworkspaces:\n    - name: platform-core\n      source: obin-ai/platform-core\n      version: " + importedPlatformCommit + "\nmodules:\n    - name: apps\n",
		filepath.Join(product, "modules/apps/module.codefly.yaml"):                "kind: module\nname: apps\nservices:\n    - name: web\n",
		filepath.Join(product, "modules/apps/services/web/service.codefly.yaml"):  "kind: service\nname: web\nversion: 0.0.0\n" + agent + "endpoints:\n    - name: http\n      api: http\nservice-dependencies:\n    - interface: example.dev/widgets@^1.1\n",
	}
	for path, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return product
}

// A `workspaces:` import naming a source and a commit pin resolves through the
// module cache a module pin uses, with no `path:` override, and the service
// loaded through that composition carries the binding the host hands its
// agent (ServiceIdentity.interface_bindings): the agent never has to compose
// the import itself.
func TestResolvedWorkspaceImportBindsTheServiceForItsAgent(t *testing.T) {
	product := importingWorkspace(t)
	ctx := WithWorkspaceResolver(context.Background())
	workspace, err := resources.LoadWorkspaceFromDir(ctx, product)
	require.NoError(t, err)
	web, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Module: "apps", Name: "web"})
	require.NoError(t, err)
	supplied := web.InterfaceBindingsProto()
	require.NotNil(t, supplied, "the host's composition bound the import's provider")
	require.Len(t, supplied.Bindings, 1)
	require.Equal(t, "example.dev/widgets@^1.1", supplied.Bindings[0].Interface)
	require.Equal(t, "platform", supplied.Bindings[0].Module)
	require.Equal(t, "api", supplied.Bindings[0].Service)
	require.Equal(t, "grpc", supplied.Bindings[0].Endpoint)
}

// Without the resolver the import is genuinely unresolvable, and core's refusal
// stands: that is the sentence an agent process, which registers no resolver,
// reports when it loads the workspace itself.
func TestUnresolvedWorkspaceImportRequiresHostResolution(t *testing.T) {
	product := importingWorkspace(t)
	_, err := resources.LoadWorkspaceFromDir(context.Background(), product)
	require.ErrorContains(t, err, `workspace "platform-core" at obin-ai/platform-core@`+importedPlatformCommit+" requires host resolution")
}
