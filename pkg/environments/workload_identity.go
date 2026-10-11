package environments

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// WorkloadIdentityConfiguration is the service opt-in to projected workload credentials.
const WorkloadIdentityConfiguration = "workload-identity"

// ModuleAuthorityConfiguration requests per-workload authority from the render.
const ModuleAuthorityConfiguration = "module-authority"

// RenderOwnedWorkspaceConfiguration names the groups supplied per workload by
// the render. They are never dependencies of the configuration manager and
// cannot be provided by authored workspace configuration files.
func RenderOwnedWorkspaceConfiguration(group string) bool {
	return group == WorkloadIdentityConfiguration || group == ModuleAuthorityConfiguration
}

// WorkloadIdentityServices selects explicit consumers, never composition-root
// inheritance, and validates the gateway against the same complete composition.
func WorkloadIdentityServices(ctx context.Context, workspace *resources.Workspace, env *Environment) ([]*resources.ServiceWithModule, error) {
	if workspace == nil || env == nil {
		return nil, fmt.Errorf("workload identities require a workspace and environment")
	}
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return nil, err
	}
	var consumers []*resources.ServiceWithModule
	gatewayFound := false
	for _, ref := range refs {
		if env.Host != nil && resources.ServiceUnique(ref.Module, ref.Name) == env.Host.Gateway {
			gatewayFound = true
		}
		service, err := workspace.LoadService(ctx, ref)
		if err != nil {
			return nil, err
		}
		if slices.Contains(service.WorkspaceConfigurationDependencies, ModuleAuthorityConfiguration) {
			if !slices.Contains(service.WorkspaceConfigurationDependencies, WorkloadIdentityConfiguration) {
				return nil, fmt.Errorf("service %s/%s declares module-authority but must also declare workload-identity", ref.Module, ref.Name)
			}
			if env.Host == nil || env.Host.Audience == "" {
				return nil, fmt.Errorf("service %s/%s declares module-authority, which requires host.audience", ref.Module, ref.Name)
			}
		}
		if slices.Contains(service.WorkspaceConfigurationDependencies, WorkloadIdentityConfiguration) {
			if _, managed := env.ManagedService(ref.Module, ref.Name); managed {
				return nil, fmt.Errorf("managed service %s/%s declares workload-identity but has no local workload", ref.Module, ref.Name)
			}
			consumers = append(consumers, ref)
		}
	}
	if len(consumers) > 0 && (env.Host == nil || env.Host.Gateway == "") {
		return nil, fmt.Errorf("environment %s requires host.gateway (<module>/<service>) because composed services declare workload-identity", env.Name)
	}
	if env.Host != nil && env.Host.Gateway != "" {
		if err := env.Host.Validate(); err != nil {
			return nil, err
		}
		if !gatewayFound {
			return nil, fmt.Errorf("host.gateway %q names no composed service", env.Host.Gateway)
		}
		module, service, _ := strings.Cut(env.Host.Gateway, "/")
		if _, managed := env.ManagedService(module, service); managed {
			return nil, fmt.Errorf("host.gateway %q is a managed service with no composed workload identity", env.Host.Gateway)
		}
	}
	return consumers, nil
}
