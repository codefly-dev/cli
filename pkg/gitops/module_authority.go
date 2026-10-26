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

// meshPrincipal is the SPIFFE ID the cell's mesh issues to a workload: the one
// of the ServiceAccount Core names for the service, in the module's namespace,
// under the host's trust domain. It is the same identity a module manifest
// names per workload (renderedIdentityWorkload reads the account off the
// rendered pod and asks the same function), and the only transport identity a workload has:
// the mesh presents it, the workload never does.
func meshPrincipal(host *environments.EnvironmentHost, namespace, service string) string {
	return host.SPIFFEID(namespace, workloadAccount(service))
}

// moduleAuthorityInjection projects the module-authority group for a service
// that declares it: its mesh principal, the two audiences the host names, and
// the internal token aliased to the existing internal-auth secret key rather
// than a second credential or its value. Core encodes the SDK carriers.
func moduleAuthorityInjection(service *resources.Service, env *environments.Environment, scope unitScope, injection serviceInjection) (serviceInjection, error) {
	if !slices.Contains(service.WorkspaceConfigurationDependencies, environments.ModuleAuthorityConfiguration) {
		return injection, nil
	}
	unique := resources.ServiceUnique(scope.Module, service.Name)
	if env.Host == nil || env.Host.Audience == "" {
		return serviceInjection{}, fmt.Errorf("module-authority service %s requires host.audience", unique)
	}
	if env.Host.ModuleAudience == "" {
		return serviceInjection{}, fmt.Errorf("module-authority service %s requires host.module_audience", unique)
	}
	if scope.Module == "" || scope.Namespace == "" {
		return serviceInjection{}, fmt.Errorf("module-authority service %s has no resolved namespace, so its mesh principal cannot be named", unique)
	}
	configuration := &basev0.Configuration{Origin: resources.ConfigurationWorkspace}
	carrier := func(group, key string, secret bool) string {
		return resources.ConfigurationValueEnvironmentKey(configuration, group, &basev0.ConfigurationValue{Key: key, Secret: secret})
	}
	public := map[string]string{}
	for key, value := range map[string]string{
		"PRINCIPAL": meshPrincipal(env.Host, scope.Namespace, service.Name),
		// The host's name for a module capability's audience, never the
		// module's own: the module holds its credential to this value, and
		// the host stamps one audience on every module's.
		"AUDIENCE":            env.Host.ModuleAudience,
		"PROJECTION_AUDIENCE": env.Host.Audience,
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
