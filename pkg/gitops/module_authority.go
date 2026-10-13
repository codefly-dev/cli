package gitops

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// The render owns these groups, including when an authored group would agree
// or no service declares it as a dependency.
// Read through Core so composed modules, imports and profile selection follow
// the same rules as every other workspace configuration.
func refuseAuthoredRenderConfigurations(ctx context.Context, workspace *resources.Workspace, env *resources.Environment) error {
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env)
	if err != nil {
		return err
	}
	for _, info := range provided.Infos {
		if environments.RenderOwnedWorkspaceConfiguration(info.Name) {
			return fmt.Errorf("workspace configuration %q is owned by the render; remove the authored group file from the composition", info.Name)
		}
	}
	for _, group := range slices.Sorted(maps.Keys(provided.Ambiguous)) {
		if environments.RenderOwnedWorkspaceConfiguration(group) {
			return fmt.Errorf("workspace configuration %q is owned by the render; remove the authored group file from the composition", group)
		}
	}
	return nil
}

// moduleAuthorityInjection uses the same composition module and resolved peer
// as presence. projectWorkloadSVID verifies that the effective serving pods run
// under this peer's account before the render can succeed. Core encodes the SDK
// carriers, and the token aliases the existing internal-auth secret key rather
// than introducing a second credential or reading its value.
func moduleAuthorityInjection(service *resources.Service, env *environments.Environment, scope unitScope, injection serviceInjection) (serviceInjection, error) {
	if !slices.Contains(service.WorkspaceConfigurationDependencies, environments.ModuleAuthorityConfiguration) {
		return injection, nil
	}
	unique := resources.ServiceUnique(scope.Module, service.Name)
	if !requestsWorkloadIdentity(service) {
		return serviceInjection{}, fmt.Errorf("module-authority service %s must also declare workload-identity", unique)
	}
	if env.Host == nil || env.Host.Audience == "" {
		return serviceInjection{}, fmt.Errorf("module-authority service %s requires host.audience", unique)
	}
	peer, exists := scope.WorkloadPeers[unique]
	if scope.Module == "" || !exists || !peer.Consumer || peer.SPIFFEID == "" {
		return serviceInjection{}, fmt.Errorf("module-authority service %s has no composition identity plan", unique)
	}
	configuration := &basev0.Configuration{Origin: resources.ConfigurationWorkspace}
	carrier := func(group, key string, secret bool) string {
		return resources.ConfigurationValueEnvironmentKey(configuration, group, &basev0.ConfigurationValue{Key: key, Secret: secret})
	}
	public := map[string]string{}
	for key, value := range map[string]string{
		"PRINCIPAL": peer.SPIFFEID, "AUDIENCE": scope.Module, "PROJECTION_AUDIENCE": env.Host.Audience,
	} {
		public[carrier(environments.ModuleAuthorityConfiguration, key, false)] = value
	}
	secrets := map[string]string{
		carrier(environments.ModuleAuthorityConfiguration, "INTERNAL_TOKEN", true): carrier("internal-auth", "CODEFLY_INTERNAL_TOKEN", true),
	}
	for _, values := range []map[string]string{public, secrets} {
		for key := range values {
			if _, exists := injection.Public[key]; exists {
				return serviceInjection{}, fmt.Errorf("service %s already derives render-owned key %s", unique, key)
			}
			if _, exists := injection.Secrets[key]; exists {
				return serviceInjection{}, fmt.Errorf("service %s already derives render-owned key %s", unique, key)
			}
		}
	}
	maps.Copy(public, injection.Public)
	maps.Copy(secrets, injection.Secrets)
	return serviceInjection{Public: public, Secrets: secrets}, nil
}
