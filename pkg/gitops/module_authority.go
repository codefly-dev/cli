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

// The render owns this group, including when an authored group would agree.
// Read through Core so composed modules, imports and profile selection follow
// the same rules as every other workspace configuration.
func refuseAuthoredModuleAuthority(ctx context.Context, workspace *resources.Workspace, env *resources.Environment) error {
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env)
	if err != nil {
		return err
	}
	group := environments.ModuleAuthorityConfiguration
	if _, ambiguous := provided.Ambiguous[group]; ambiguous || slices.ContainsFunc(provided.Infos, func(info *basev0.ConfigurationInformation) bool {
		return info.Name == group
	}) {
		return fmt.Errorf("workspace configuration %q is owned by the render; remove the authored group file from the composition", group)
	}
	return nil
}

// moduleAuthorityLoader declares the render-owned dependency to Core without
// inventing a workspace-wide principal. Its values are supplied per service by
// moduleAuthorityInjection once the composition's workload identities resolve.
type moduleAuthorityLoader struct {
	workspace *resources.Workspace
	configs   []*basev0.Configuration
}

func newModuleAuthorityLoader(workspace *resources.Workspace) *moduleAuthorityLoader {
	return &moduleAuthorityLoader{workspace: workspace, configs: []*basev0.Configuration{{
		Origin: resources.ConfigurationWorkspace,
		Infos:  []*basev0.ConfigurationInformation{{Name: environments.ModuleAuthorityConfiguration}},
	}}}
}

func (*moduleAuthorityLoader) Identity() string { return "render-module-authority" }

func (loader *moduleAuthorityLoader) Load(ctx context.Context, env *resources.Environment) error {
	return refuseAuthoredModuleAuthority(ctx, loader.workspace, env)
}

func (loader *moduleAuthorityLoader) Configurations() []*basev0.Configuration { return loader.configs }

func (*moduleAuthorityLoader) DNS() []*basev0.DNS { return nil }

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
