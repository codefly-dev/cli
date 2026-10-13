package environments

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// ModuleAuthorityConfiguration requests per-workload authority from the render.
const ModuleAuthorityConfiguration = "module-authority"

// RenderOwnedWorkspaceConfiguration names the groups supplied per workload by
// the render. They are never dependencies of the configuration manager and
// cannot be provided by authored workspace configuration files.
func RenderOwnedWorkspaceConfiguration(group string) bool {
	return group == ModuleAuthorityConfiguration
}

// ValidateHostComposition checks what a host block asks of the composition: a
// `host.gateway` names a composed service with a workload of its own (a
// managed service has none to front solutions with), and a service declaring
// module-authority has the two audiences the render projects for it. A
// workload's transport identity is the cell's mesh principal — the SPIFFE ID
// of its ServiceAccount — which no service opts into and no render issues, so
// nothing here selects consumers of anything.
func ValidateHostComposition(ctx context.Context, workspace *resources.Workspace, env *Environment) error {
	if workspace == nil || env == nil {
		return fmt.Errorf("a host composition requires a workspace and environment")
	}
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return err
	}
	gatewayFound := false
	for _, ref := range refs {
		if env.Host != nil && resources.ServiceUnique(ref.Module, ref.Name) == env.Host.Gateway {
			gatewayFound = true
		}
		service, err := workspace.LoadService(ctx, ref)
		if err != nil {
			return err
		}
		if slices.Contains(service.WorkspaceConfigurationDependencies, ModuleAuthorityConfiguration) {
			if env.Host == nil || env.Host.Audience == "" {
				return fmt.Errorf("service %s/%s declares module-authority, which requires host.audience", ref.Module, ref.Name)
			}
			if env.Host.ModuleAudience == "" {
				return fmt.Errorf("service %s/%s declares module-authority, which requires host.module_audience: the audience the host stamps on a module's own capability, as the host names it", ref.Module, ref.Name)
			}
		}
	}
	if env.Host != nil && env.Host.Gateway != "" {
		if err := env.Host.Validate(); err != nil {
			return err
		}
		if !gatewayFound {
			return fmt.Errorf("host.gateway %q names no composed service", env.Host.Gateway)
		}
		module, service, _ := strings.Cut(env.Host.Gateway, "/")
		if _, managed := env.ManagedService(module, service); managed {
			return fmt.Errorf("host.gateway %q is a managed service with no composed workload", env.Host.Gateway)
		}
	}
	return nil
}
