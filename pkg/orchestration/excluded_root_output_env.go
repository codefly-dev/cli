package orchestration

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// exportExcludedOriginEnvironment writes the owner-only SDK environment for an
// excluded root after its real dependency graph has reached the RuntimeStart
// barrier. It deliberately exports no agent-produced runtime configuration:
// --exclude-root guarantees that the root agent was never loaded. Service and
// workspace configuration, dependency configuration, dependency endpoints,
// invocation overrides, and runtime identity remain authoritative because all
// are composed by the same Codefly managers used for a running service.
//
// ARCHITECTURE: This is the dependency-composition boundary for tools that need
// a service's Codefly SDK context without starting that service. It must never
// shell-author credentials or ask callers to copy secrets into a second path.
func (flow *Flow) exportExcludedOriginEnvironment(ctx context.Context) error {
	if !flow.exportsExcludedOriginEnvironment() {
		return nil
	}
	if flow.world == nil || flow.ConfigurationManager == nil || flow.SharedState == nil {
		return fmt.Errorf("excluded root environment requires initialized orchestration state")
	}

	identity, err := flow.originService.Identity()
	if err != nil {
		return fmt.Errorf("resolve excluded root identity: %w", err)
	}
	runtimeContext, err := resources.NewRuntimeContext(flow.runtimeContextFor(flow.originService))
	if err != nil {
		return fmt.Errorf("resolve excluded root runtime context: %w", err)
	}
	serviceConfiguration, err := flow.ConfigurationManager.GetServiceConfiguration(ctx, identity)
	if err != nil {
		return fmt.Errorf("load excluded root service configuration: %w", err)
	}
	workspaceConfigurations, err := flow.world.workspaceConfigurationsFor(ctx, flow.originService)
	if err != nil {
		return fmt.Errorf("load excluded root workspace configurations: %w", err)
	}
	dependencyConfigurations, err := flow.SharedState.GetDependentConfigurationsFor(ctx, identity)
	if err != nil {
		return fmt.Errorf("load excluded root dependency configurations: %w", err)
	}
	dependencyMappings, err := flow.SharedState.GetDependenciesNetworkMappings(ctx, flow.originService)
	if err != nil {
		return fmt.Errorf("load excluded root dependency endpoints: %w", err)
	}

	if err := AppendServiceProcessConfigurationsToFile(
		ctx,
		flow.outputEnvPath,
		runtimeContext,
		serviceConfiguration,
		workspaceConfigurations,
		dependencyConfigurations,
		nil,
	); err != nil {
		return fmt.Errorf("write excluded root configurations: %w", err)
	}

	protoIdentity := &basev0.ServiceIdentity{
		Workspace:           identity.Workspace,
		Module:              identity.Module,
		Name:                identity.Name,
		Version:             identity.Version,
		WorkspacePath:       identity.WorkspacePath,
		RelativeToWorkspace: identity.RelativeToWorkspace,
	}
	overrides := make(map[string]string, len(flow.overrides[flow.originService.Name])+1)
	for key, value := range flow.overrides[flow.originService.Name] {
		overrides[key] = value
	}
	if flow.world.Env != nil && flow.world.Env.NamingScope != "" {
		overrides[resources.NamingScopePrefix] = flow.world.Env.NamingScope
	}
	if err := AppendRuntimeEnvironmentToFile(
		ctx,
		flow.outputEnvPath,
		protoIdentity,
		runtimeContext,
		flow.fixture,
		overrides,
		dependencyMappings,
	); err != nil {
		return fmt.Errorf("write excluded root runtime environment: %w", err)
	}
	return nil
}
