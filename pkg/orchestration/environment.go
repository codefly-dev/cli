package orchestration

import (
	"fmt"

	"github.com/codefly-dev/core/resources"
)

// LocalEnvironmentName is the environment every workspace-bound lifecycle
// command runs against unless the user selects another one with --env.
const LocalEnvironmentName = "local"

// SelectEnvironment is the canonical environment-selection path for
// workspace-bound flows. It honors the workspace's declared environment when
// present (FindEnvironment keeps the legacy synthetic "local" default for
// workspaces that never declared one) and fails — before any agent is
// spawned — when a non-local environment is requested but not declared.
//
// The returned Environment is a deep copy: invocation-scoped overrides such
// as --naming-scope must not leak into the declaration shared by concurrent
// flows over the same Workspace.
func SelectEnvironment(workspace *resources.Workspace, name string) (*resources.Environment, error) {
	env := workspace.FindEnvironment(name)
	if env == nil {
		return nil, fmt.Errorf("workspace %q does not declare environment %q in %s", workspace.Name, name, resources.WorkspaceConfigurationName)
	}
	return cloneEnvironment(env), nil
}

// SelectedFixture resolves the fixture a workspace-bound flow runs with. An
// environment declares the fixture its runtime should use, so an ordinary
// `codefly run service` / `codefly test service` needs no --fixture at all.
// An explicit override always wins, and an environment that deliberately
// omits a fixture — because it loads a real provider — resolves to none.
func SelectedFixture(environment *resources.Environment, override string) string {
	if override != "" {
		return override
	}
	if environment == nil {
		return ""
	}
	return environment.Fixture
}

func cloneEnvironment(env *resources.Environment) *resources.Environment {
	clone := *env
	if env.Cluster != nil {
		cluster := *env.Cluster
		clone.Cluster = &cluster
	}
	if env.Registry != nil {
		registry := *env.Registry
		clone.Registry = &registry
	}
	if env.Gitops != nil {
		gitops := *env.Gitops
		clone.Gitops = &gitops
	}
	if len(env.Ingress) > 0 {
		clone.Ingress = append([]resources.EnvironmentIngressRoute(nil), env.Ingress...)
		for i := range clone.Ingress {
			clone.Ingress[i].Hosts = append([]string(nil), env.Ingress[i].Hosts...)
		}
	}
	if len(env.ManagedServices) > 0 {
		clone.ManagedServices = make(map[string]resources.EnvironmentManagedService, len(env.ManagedServices))
		for name, managed := range env.ManagedServices {
			managed.EgressCIDRs = append([]string(nil), managed.EgressCIDRs...)
			managed.SecretReferences = append([]resources.EnvironmentManagedSecretReference(nil), managed.SecretReferences...)
			clone.ManagedServices[name] = managed
		}
	}
	if len(env.Secrets) > 0 {
		clone.Secrets = make([]*resources.EnvironmentSecretProvider, len(env.Secrets))
		for i, provider := range env.Secrets {
			p := *provider
			clone.Secrets[i] = &p
		}
	}
	return &clone
}
