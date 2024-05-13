package architecture_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

func createServices(services ...string) []architecture.Service {
	var out []architecture.Service
	for _, service := range services {
		out = append(out, architecture.Service{Unique: service})
	}
	return out
}

// Modules Layout:
// management:
// - organization
// billing
// - accounts -> management/organization
// web:
// - gateway  -> management/organization
// - gateway  -> billing/accounts
// - frontend -> gateway

// Flat Layout:
// organization
// accounts -> organization
// gateway -> organization
// gateway -> accounts
// frontend -> gateway

func TestServiceDependenciesModulesLayout(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)

	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/module-layout")
	require.NoError(t, err)
	require.NotNil(t, workspace)
	require.Equal(t, 3, len(workspace.Modules))

	organization := shared.Must(workspace.FindUniqueServiceByName(ctx, "management/organization"))
	require.NotNil(t, organization)
	accounts := shared.Must(workspace.FindUniqueServiceByName(ctx, "billing/accounts"))
	require.NotNil(t, accounts)
	gateway := shared.Must(workspace.FindUniqueServiceByName(ctx, "web/gateway"))
	require.NotNil(t, gateway)
	frontend := shared.Must(workspace.FindUniqueServiceByName(ctx, "web/frontend"))
	require.NotNil(t, frontend)

	testServiceGraph(t, workspace, organization.Unique(), accounts.Unique(), gateway.Unique(), frontend.Unique())
}

func TestServiceDependenciesFlatLayout(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)

	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/flat-layout")
	require.NoError(t, err)
	require.NotNil(t, workspace)

	organization := shared.Must(workspace.FindUniqueServiceByName(ctx, "organization"))
	require.NotNil(t, organization)
	accounts := shared.Must(workspace.FindUniqueServiceByName(ctx, "accounts"))
	require.NotNil(t, accounts)
	gateway := shared.Must(workspace.FindUniqueServiceByName(ctx, "gateway"))
	require.NotNil(t, gateway)
	frontend := shared.Must(workspace.FindUniqueServiceByName(ctx, "frontend"))
	require.NotNil(t, frontend)

	testServiceGraph(t, workspace, organization.Unique(), accounts.Unique(), gateway.Unique(), frontend.Unique())
}

func testServiceGraph(t *testing.T, workspace *resources.Workspace, organization, accounts, gateway, frontend string) {
	ctx := context.Background()
	dep, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	require.NotNil(t, dep)

	for _, service := range []string{organization, accounts, gateway, frontend} {
		svc, err := dep.ServiceFromUnique(service)
		require.NoError(t, err)
		require.NotNil(t, svc)
		require.Equal(t, service, svc.Unique())
	}

	require.Equal(t, 4, len(dep.Services()))

	require.Equal(t, 4, len(dep.Dependencies()))

	// Sanity checks
	ok, err := dep.DependsOn(accounts, organization)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = dep.DependsOn(gateway, organization)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = dep.DependsOn(gateway, accounts)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = dep.DependsOn(frontend, organization)
	require.NoError(t, err)
	require.True(t, ok)

	// Check Restrict
	smallerDep, err := dep.Restrict(ctx, accounts)
	require.NoError(t, err)

	require.ElementsMatch(t, createServices(accounts, organization), smallerDep.Services())

	// Check DirectRequires

	deps, err := dep.DirectRequires(ctx, organization)
	require.NoError(t, err)
	require.Equal(t, 0, len(deps))

	deps, err = dep.DirectRequires(ctx, accounts)
	require.NoError(t, err)
	require.Equal(t, createServices(organization), deps)

	deps, err = dep.DirectRequires(ctx, gateway)
	require.NoError(t, err)
	require.Equal(t, createServices(organization, accounts), deps)

	deps, err = dep.DirectRequires(ctx, frontend)
	require.NoError(t, err)
	require.Equal(t, createServices(gateway), deps)

	// Check DirectDependents

	reqs, err := dep.DirectDependents(ctx, frontend)
	require.NoError(t, err)
	require.Equal(t, 0, len(reqs))

	reqs, err = dep.DirectDependents(ctx, gateway)
	require.NoError(t, err)
	require.Equal(t, createServices(frontend), reqs)

	reqs, err = dep.DirectDependents(ctx, organization)
	require.NoError(t, err)
	require.Equal(t, createServices(accounts, gateway), reqs)

	// Topological sorts

	order, err := dep.OrderTo(ctx, organization)
	require.NoError(t, err)
	require.Equal(t, 0, len(order))

	order, err = dep.OrderTo(ctx, accounts)
	require.NoError(t, err)
	expected := []architecture.Service{
		{organization},
	}
	require.Equal(t, expected, order)

	order, err = dep.OrderTo(ctx, gateway)
	require.NoError(t, err)
	expected = []architecture.Service{
		{organization},
		{accounts},
	}
	require.Equal(t, expected, order)

	order, err = dep.OrderTo(ctx, frontend)
	require.NoError(t, err)
	expected = []architecture.Service{
		{organization},
		{accounts},
		{gateway},
	}
	require.Equal(t, expected, order)

}
