package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corenetwork "github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

// writeDeclaredPortsWorkspace is writeDevWorkspace where api exposes a
// conventional grpc endpoint and a named "authority" grpc sibling, and declares
// declaration (a YAML block) under spec.deployment.endpoint-ports.
func writeDeclaredPortsWorkspace(t *testing.T, declaration string) (*resources.Workspace, *resources.Module) {
	t.Helper()
	workspace, _ := writeDevWorkspace(t)
	api := devServiceYAML("api") + `endpoints:
  - name: grpc
    api: grpc
  - name: authority
    api: grpc
`
	if declaration != "" {
		api += "spec:\n  deployment:\n    endpoint-ports:\n" + declaration
	}
	full := filepath.Join(workspace.Dir(), "modules", "shop", "services", "api", resources.ServiceConfigurationName)
	require.NoError(t, os.WriteFile(full, []byte(api), 0o644))
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	return workspace, module
}

func allocatedAuthorityPort(t *testing.T) uint16 {
	t.Helper()
	ports, err := corenetwork.DeployedEndpointPorts(context.Background(), "shop", "api", []*basev0.Endpoint{
		{Module: "shop", Service: "api", Name: "grpc", Api: standards.GRPC},
		{Module: "shop", Service: "api", Name: "authority", Api: standards.GRPC},
	})
	require.NoError(t, err)
	return ports["authority"]
}

// A declared port that disagrees with the allocation refuses the render before
// the registry is prepared (this environment declares none, which would
// otherwise be the error), so nothing was built or pushed.
func TestRenderModuleRefusesAMismatchedDeclaredEndpointPort(t *testing.T) {
	allocated := allocatedAuthorityPort(t)
	declared := allocated + 1
	workspace, module := writeDeclaredPortsWorkspace(t, fmt.Sprintf("      grpc: 9090\n      authority: %d\n", declared))
	_, err := RenderModule(context.Background(), workspace, module, environmentNamed("staging"), "acme-staging", nil)
	require.Error(t, err)
	require.ErrorContains(t, err, fmt.Sprintf(
		"service shop/api endpoint authority: declared port %d (spec.deployment.endpoint-ports) differs from the allocated in-cluster port %d",
		declared, allocated))
	require.NotContains(t, err.Error(), "endpoint grpc:", "the matching grpc declaration is not reported")
	require.NotContains(t, err.Error(), "registry.url")
}

// A declaration naming an endpoint the render does not place is refused too.
func TestRenderModuleRefusesADeclaredPortForAnUnknownEndpoint(t *testing.T) {
	workspace, module := writeDeclaredPortsWorkspace(t, "      missing: 9000\n")
	_, err := RenderModule(context.Background(), workspace, module, environmentNamed("staging"), "acme-staging", nil)
	require.ErrorContains(t, err, `service shop/api declares spec.deployment.endpoint-ports.missing = 9000, but the render places no in-cluster endpoint "missing"`)
}

// A declaration matching the allocation, and no declaration at all, pass the
// check: the render goes on to the next step and stops there, at the registry.
func TestRenderModuleAcceptsAMatchingOrAbsentDeclaredEndpointPort(t *testing.T) {
	for name, declaration := range map[string]string{
		"matching": fmt.Sprintf("      grpc: 9090\n      authority: %d\n", allocatedAuthorityPort(t)),
		"absent":   "",
	} {
		t.Run(name, func(t *testing.T) {
			workspace, module := writeDeclaredPortsWorkspace(t, declaration)
			_, err := RenderModule(context.Background(), workspace, module, environmentNamed("staging"), "acme-staging", nil)
			require.ErrorContains(t, err, "must declare registry.url")
			require.NotContains(t, err.Error(), "endpoint-ports")
		})
	}
}
