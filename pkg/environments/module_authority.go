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

// ArtifactCacheConfiguration is the render-owned group naming where a host
// reads frontend bundles from, by the digest a signed module manifest names.
// A NATIVE run hands ArtifactCacheRootKey, the workspace's artifact cache the
// CLI retains bundles in; a deployed render hands ArtifactCacheRepositoryKey,
// the one repository it publishes bundles to. A service receives exactly one.
const ArtifactCacheConfiguration = "artifact-cache"

// ArtifactCacheRootKey is the native value of ArtifactCacheConfiguration: the
// absolute path of the cache directory.
const ArtifactCacheRootKey = "ROOT"

// ArtifactCacheRepositoryKey is the deployed value of
// ArtifactCacheConfiguration: the OCI repository (registry host and path, no
// tag or digest) the render publishes every frontend bundle to.
const ArtifactCacheRepositoryKey = "REPOSITORY"

// FederationConfiguration is the host's authored workspace configuration
// group (its registration secrets live there, declared by the composition).
// Three of its keys turn the host's delivery surface on and say what it
// answers for; a cell's composition declares them for its environment, and a
// NATIVE run derives them from the environment's host block for the delivery
// host alone, since the run is the one that knows the host it stands up.
// The group stays authored: it is never render-owned.
const FederationConfiguration = "federation"

// The federation keys a host reads to answer for a coordinate: the coordinate
// itself, the ownership domains it accepts a binding under, and how it checks
// a delivered carrier, for which the only value is pinned keys.
const (
	FederationHostCoordinateKey       = "SOLUTION_HOST_COORDINATE"
	FederationHostOwnershipDomainsKey = "SOLUTION_HOST_OWNERSHIP_DOMAINS"
	FederationHostTrustPolicyKey      = "SOLUTION_HOST_TRUST_POLICY"
	FederationHostTrustPolicyKeys     = "keys"
)

// RenderOwnedWorkspaceConfiguration names the groups supplied per workload by
// the render (or by the native run standing in for it). They are never
// dependencies of the configuration manager and cannot be provided by
// authored workspace configuration files.
func RenderOwnedWorkspaceConfiguration(group string) bool {
	return group == ModuleAuthorityConfiguration || group == ArtifactCacheConfiguration
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
