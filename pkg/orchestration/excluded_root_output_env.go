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
	dependencyMappings, err := flow.SharedState.GetDependenciesNetworkMappings(ctx, flow.originService)
	if err != nil {
		return fmt.Errorf("load excluded root dependency endpoints: %w", err)
	}
	workspaceConfigurations, err := flow.world.workspaceConfigurationsFor(ctx, flow.originService,
		dependencyMappings, resources.NetworkAccessFromRuntimeContext(runtimeContext))
	if err != nil {
		return fmt.Errorf("load excluded root workspace configurations: %w", err)
	}
	dependencyConfigurations, err := flow.SharedState.GetDependentConfigurationsFor(ctx, identity)
	if err != nil {
		return fmt.Errorf("load excluded root dependency configurations: %w", err)
	}
	// --exclude-root never loads the root agent, so services.RuntimeInstance.Start
	// — the call that narrows what a running service receives — never runs for it.
	// This file is then the ONLY carrier of its dependencies' addresses, and
	// narrowing it here is the only enforcement there is. identity.Module is the
	// consumer module the runner passes as instance.Module.Name: core builds the
	// identity from that same field, so the two carriers agree by construction.
	endpointMappings, err := outputEnvNetworkMappings(
		flow.world.Workspace,
		identity.Module,
		flow.originService.ServiceDependencies,
		nil,
		dependencyMappings,
	)
	if err != nil {
		return fmt.Errorf("resolve excluded root dependency endpoints: %w", err)
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
	overrides := flow.DependencyProcessVariables(flow.originService)
	if err := AppendRuntimeEnvironmentToFile(
		ctx,
		flow.outputEnvPath,
		protoIdentity,
		runtimeContext,
		flow.fixture,
		overrides,
		endpointMappings,
	); err != nil {
		return fmt.Errorf("write excluded root runtime environment: %w", err)
	}
	return nil
}

// prepareExcludedOrigin runs at the dependency barrier without loading an agent.
func (flow *Flow) prepareExcludedOrigin(ctx context.Context) error {
	flow.externalPrepared.Store(false)
	var abandon func()
	handed := false
	defer func() {
		if !handed && abandon != nil {
			abandon()
		}
	}()
	if flow.externalIdentityOrigin() {
		if flow.world.PrepareExternalWorkload == nil {
			return fmt.Errorf("excluded root declares module-identity but this run supplies no workload authority")
		}
		var err error
		abandon, err = flow.world.PrepareExternalWorkload(ctx, flow.originService)
		if err != nil {
			return err
		}
	}
	if err := flow.exportExcludedOriginEnvironment(ctx); err != nil {
		return err
	}
	handed = true
	flow.externalPrepared.Store(true)
	return nil
}

// DependencyProcessVariables carries the same per-service overrides as a native
// process to the dependency SDK. It exports no other service's values.
func (flow *Flow) DependencyProcessVariables(service *resources.Service) map[string]string {
	out := map[string]string{}
	for key, value := range flow.overridesFor(service) {
		out[key] = value
	}
	if flow.world != nil {
		if flow.world.RuntimeOverrides != nil {
			for key, value := range flow.world.RuntimeOverrides(service) {
				out[key] = value
			}
		}
		if flow.world.Env != nil {
			out[resources.EnvironmentPrefix] = flow.world.Env.Name
			if flow.world.Env.NamingScope != "" {
				out[resources.NamingScopePrefix] = flow.world.Env.NamingScope
			}
		}
	}
	return out
}
