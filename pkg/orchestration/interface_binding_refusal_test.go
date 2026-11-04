package orchestration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

// bindingWorkspace writes a workspace where apps/web requires an interface
// platform/api implements, so a load through the workspace binds it.
func bindingWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	agent := "agent:\n    kind: codefly:service\n    name: go\n    version: 0.0.67\n    publisher: codefly.dev\n"
	files := map[string]string{
		"workspace.codefly.yaml":                             "name: product\nlayout: modules\nmodules:\n    - name: platform\n    - name: apps\n",
		"modules/platform/module.codefly.yaml":               "kind: module\nname: platform\nservices:\n    - name: api\ninterface:\n    endpoints:\n        - service: api\n          endpoint: grpc\n          visibility: public\n          implements:\n              - example.dev/widgets@1.2.0\n",
		"modules/platform/services/api/service.codefly.yaml": "kind: service\nname: api\nversion: 0.0.0\n" + agent + "endpoints:\n    - name: grpc\n      api: grpc\n",
		"modules/apps/module.codefly.yaml":                   "kind: module\nname: apps\nservices:\n    - name: web\n",
		"modules/apps/services/web/service.codefly.yaml":     "kind: service\nname: web\nversion: 0.0.0\n" + agent + "endpoints:\n    - name: http\n      api: http\nservice-dependencies:\n    - interface: example.dev/widgets@^1.1\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return root
}

// The agent's refusal is core's sentence about the host. When this host did
// supply the binding, the error names the agent that ignored it and the
// release floor that reads it, and keeps the agent's own error underneath.
func TestExplainHostResolutionRefusalNamesTheAgentBehindTheBinding(t *testing.T) {
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, bindingWorkspace(t))
	require.NoError(t, err)
	bound, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Module: "apps", Name: "web"})
	require.NoError(t, err)
	require.NotNil(t, bound.InterfaceBindingsProto(), "the host's composition bound web's interface dependency")
	identity, err := bound.Identity()
	require.NoError(t, err)
	instance := &coreservices.Instance{Service: bound, Identity: identity}

	agentErr := errors.New(`cannot bind interface dependencies: resources.ApplyInterfaceBindings: cannot load the workspace that binds the service's interfaces: LoadFromDir: workspace "platform-core" at obin-ai/platform-core@5a8d5ffa requires host resolution`)
	explained := explainHostResolutionRefusal(agentErr, instance)
	require.ErrorIs(t, explained, agentErr, "the agent's own error stays underneath")
	require.ErrorContains(t, explained, "agent codefly.dev/go:0.0.67 binds apps/web's interface dependencies against the workspace on disk")
	require.ErrorContains(t, explained, "core v0.19.8 or newer")
	require.ErrorContains(t, explained, "path: override on the import hides this")

	// The running agent, when the selection resolved one, is the one named.
	instance.Agent = &services.ServiceAgent{Agent: &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "go", Version: "0.0.74"}}
	require.ErrorContains(t, explainHostResolutionRefusal(agentErr, instance), "agent codefly.dev/go:0.0.74 binds")
}

// A service loaded outside its composition has no binding from this host, so
// the agent binding it against the workspace on disk is the contract, and its
// refusal is reported as it stands. So is any other error.
func TestExplainHostResolutionRefusalLeavesOtherErrorsAlone(t *testing.T) {
	ctx := context.Background()
	root := bindingWorkspace(t)
	unbound, err := resources.LoadServiceFromDir(ctx, filepath.Join(root, "modules/apps/services/web"))
	require.NoError(t, err)
	unbound.WithModule("apps")
	require.Nil(t, unbound.InterfaceBindingsProto(), "loaded by directory: nothing bound it")
	identity, err := unbound.Identity()
	require.NoError(t, err)
	instance := &coreservices.Instance{Service: unbound, Identity: identity}

	refusal := errors.New(`workspace "platform-core" at obin-ai/platform-core@5a8d5ffa requires host resolution`)
	require.Same(t, refusal, explainHostResolutionRefusal(refusal, instance))

	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	bound, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Module: "apps", Name: "web"})
	require.NoError(t, err)
	other := errors.New("cannot load service configuration")
	require.Same(t, other, explainHostResolutionRefusal(other, &coreservices.Instance{Service: bound, Identity: identity}))
	require.NoError(t, explainHostResolutionRefusal(nil, instance))
	require.Same(t, refusal, explainHostResolutionRefusal(refusal, nil))
}
