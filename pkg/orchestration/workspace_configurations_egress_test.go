package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// egressDeclaration is an environment's egress block naming payments/api's
// external reach, one host on the default port and one on its own.
func egressDeclaration() map[string]environments.EnvironmentEgress {
	return map[string]environments.EnvironmentEgress{
		"payments/api": {Hosts: []environments.EnvironmentEgressHost{
			{Name: "identity.example.test"},
			{Name: "api.github.com", Port: 8443},
		}},
	}
}

// declaringEgress is service as it would be had its manifest declared the
// egress group.
func declaringEgress(service *resources.Service) *resources.Service {
	declared := *service
	declared.WorkspaceConfigurationDependencies = append(append([]string(nil), service.WorkspaceConfigurationDependencies...), environments.EgressConfiguration)
	return &declared
}

// runAndRenderGroups resolves service's workspace configurations through the
// run path and the render path (the Builder a gitops render deploys through).
func runAndRenderGroups(t *testing.T, runWorld, renderWorld *World, workspace *resources.Workspace, service *resources.Service) (run, render []*basev0.Configuration) {
	t.Helper()
	ctx := context.Background()
	instance := parityInstance(t, workspace, service)
	runtimeContext, err := resources.NewRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, err)
	runMappings, err := runWorld.SharedState.GetDependenciesNetworkMappings(ctx, service)
	require.NoError(t, err)
	runner := &Runner{instance: instance, world: runWorld, runtimeContext: resources.RuntimeContextNative}
	run, err = runner.workspaceConfigurations(ctx, runMappings, runtimeContext)
	require.NoError(t, err)
	renderMappings, err := renderWorld.SharedState.GetDependenciesNetworkMappings(ctx, service)
	require.NoError(t, err)
	builder := &Builder{instance: instance, world: renderWorld}
	render, err = builder.workspaceConfigurations(ctx, renderMappings)
	require.NoError(t, err)
	return run, render
}

// A service that declares the egress group receives exactly the hosts the
// environment's egress block declares for it — host:port, sorted, every port
// explicit, the list the cell file carries — once, on the run and on the
// render alike. Nothing is authored for it: the CLI generates it.
func TestTheEnvironmentsEgressDeclarationReachesTheServiceOnRunAndRender(t *testing.T) {
	runWorld, workspace := parityWorld(t, RunMode)
	renderWorld, _ := parityWorld(t, SnapshotMode)
	runWorld.Env.Egress, renderWorld.Env.Egress = egressDeclaration(), egressDeclaration()

	service := declaringEgress(parityService(t, workspace, "payments", "api"))
	run, render := runAndRenderGroups(t, runWorld, renderWorld, workspace, service)
	for path, confs := range map[string][]*basev0.Configuration{"run": run, "render": render} {
		hosts, delivered := groupValue(confs, environments.EgressConfiguration, environments.EgressHostsKey)
		require.True(t, delivered, "the %s did not deliver egress/HOSTS", path)
		require.Equal(t, "api.github.com:8443,identity.example.test:443", hosts, "the %s delivered other hosts than the declaration", path)
		require.Equal(t, 1, groupCounts(confs)[environments.EgressConfiguration], "the %s delivered egress more than once", path)
	}

	// The process reads it under the workspace-configuration carrier every
	// SDK resolves the (group, key) pair from.
	variables, err := resources.ConfigurationAsEnvironmentVariables(runWorld.Env.EgressGroup("payments", "api"), "local", false)
	require.NoError(t, err)
	require.Len(t, variables, 1)
	require.Equal(t, "CODEFLY__WORKSPACE_CONFIGURATION__EGRESS__HOSTS", variables[0].Key)
	require.Equal(t, "api.github.com:8443,identity.example.test:443", variables[0].Value)
}

// A service that declares the group and for which the environment declares
// no egress receives it empty, on both paths: "nothing declared for you" is
// an answer, never a missing group.
func TestAServiceWithNoEgressDeclarationReceivesItEmpty(t *testing.T) {
	runWorld, workspace := parityWorld(t, RunMode)
	renderWorld, _ := parityWorld(t, SnapshotMode)
	runWorld.Env.Egress, renderWorld.Env.Egress = egressDeclaration(), egressDeclaration()

	service := declaringEgress(parityService(t, workspace, "payments", "worker"))
	run, render := runAndRenderGroups(t, runWorld, renderWorld, workspace, service)
	for path, confs := range map[string][]*basev0.Configuration{"run": run, "render": render} {
		hosts, delivered := groupValue(confs, environments.EgressConfiguration, environments.EgressHostsKey)
		require.True(t, delivered, "the %s did not deliver egress/HOSTS", path)
		require.Empty(t, hosts, "the %s delivered hosts the environment does not declare for payments/worker", path)
	}
}

// The group reaches only a service that declares it, and a run profile that
// excludes it trims it like any other group.
func TestTheEgressGroupReachesOnlyADeclaringService(t *testing.T) {
	runWorld, workspace := parityWorld(t, RunMode)
	renderWorld, _ := parityWorld(t, SnapshotMode)
	runWorld.Env.Egress, renderWorld.Env.Egress = egressDeclaration(), egressDeclaration()

	undeclared := parityService(t, workspace, "payments", "api")
	run, render := runAndRenderGroups(t, runWorld, renderWorld, workspace, undeclared)
	require.NotContains(t, groupSet(run), environments.EgressConfiguration)
	require.NotContains(t, groupSet(render), environments.EgressConfiguration)

	runWorld.excludedWorkspaceConfigurations = map[string]bool{environments.EgressConfiguration: true}
	excluded := declaringEgress(undeclared)
	instance := parityInstance(t, workspace, excluded)
	mappings, err := runWorld.SharedState.GetDependenciesNetworkMappings(context.Background(), excluded)
	require.NoError(t, err)
	runtimeContext, err := resources.NewRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, err)
	runner := &Runner{instance: instance, world: runWorld, runtimeContext: resources.RuntimeContextNative}
	run, err = runner.workspaceConfigurations(context.Background(), mappings, runtimeContext)
	require.NoError(t, err)
	require.NotContains(t, groupSet(run), environments.EgressConfiguration)
}
