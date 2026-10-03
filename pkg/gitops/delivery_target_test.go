package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeHostWorkspace adds the host's module to the cell workspace: a service
// "accounts" whose "rest" endpoint is the delivery API.
func writeHostWorkspace(t *testing.T, secured bool, withPort bool) *resources.Workspace {
	t.Helper()
	workspace := writeCellWorkspace(t)
	service := devServiceYAML("accounts") + "endpoints:\n  - name: rest\n    api: rest\n    visibility: public\n"
	if secured {
		service += "    secured: true\n"
	}
	if withPort {
		service += "spec:\n  deployment:\n    endpoint-ports:\n      rest: 8443\n"
	}
	files := map[string]string{
		filepath.Join("modules", "platform", resources.ModuleConfigurationName):                          "kind: module\nname: platform\nservices:\n  - name: accounts\n",
		filepath.Join("modules", "platform", "services", "accounts", resources.ServiceConfigurationName): service,
	}
	for rel, content := range files {
		full := filepath.Join(workspace.Dir(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	manifest, err := os.ReadFile(filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName))
	require.NoError(t, err)
	patched := strings.Replace(string(manifest), "modules:\n", "modules:\n  - name: platform\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName), []byte(patched), 0o644))
	reloaded, err := resources.LoadWorkspaceFromDir(context.Background(), workspace.Dir())
	require.NoError(t, err)
	return reloaded
}

func TestDeliveryTargetIsResolvedFromTheCompositionNeverDeclared(t *testing.T) {
	workspace := writeHostWorkspace(t, true, true)
	env := selectedEnvironment(t, workspace, "staging")
	target, err := resolveDeliveryTarget(context.Background(), workspace, env)
	require.NoError(t, err)
	// Several modules: the host's module gets its own namespace, and the
	// address follows it. The port is the Service port the CLI allocates for
	// a rest endpoint (8080), never the container port the service declares
	// (8443): the Job dials the Service, and the Service publishes the
	// allocated port in front of whatever the pod listens on.
	require.Equal(t, &DeliveryTarget{URL: "https://accounts.acme-platform.svc.cluster.local:8080", Audience: "accounts"}, target)

	plain := writeHostWorkspace(t, false, true)
	target, err = resolveDeliveryTarget(context.Background(), plain, selectedEnvironment(t, plain, "staging"))
	require.NoError(t, err)
	require.Equal(t, "http://accounts.acme-platform.svc.cluster.local:8080", target.URL)

	// A service declaring no container port still has an allocated Service
	// port: the container port is the pod's business.
	noPort := writeHostWorkspace(t, true, false)
	target, err = resolveDeliveryTarget(context.Background(), noPort, selectedEnvironment(t, noPort, "staging"))
	require.NoError(t, err)
	require.Equal(t, "https://accounts.acme-platform.svc.cluster.local:8080", target.URL)
}

func TestDeliveryTargetRefusesAnEndpointNobodyDeclared(t *testing.T) {
	// The composition has no such module at all.
	workspace := writeCellWorkspace(t)
	_, err := resolveDeliveryTarget(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "the composition does not have")

	// No host: nothing to resolve, and nothing refused.
	target, err := resolveDeliveryTarget(context.Background(), workspace, environmentNamed("staging"))
	require.NoError(t, err)
	require.Nil(t, target)
}
