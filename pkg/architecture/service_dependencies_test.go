package architecture_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/assert"
)

func createServices(services ...string) []architecture.Service {
	var out []architecture.Service
	for _, service := range services {
		out = append(out, architecture.Service{Unique: service})
	}
	return out
}

func TestServiceGraph(t *testing.T) {
	ctx := context.Background()
	wool.SetGlobalLogLevel(wool.DEBUG)

	workspace := &configurations.Workspace{}
	project, err := workspace.LoadProjectFromDir(ctx, "testdata/codefly-platform")
	assert.NoError(t, err)
	assert.NotNil(t, project)

	// applications:
	// management:
	// - organization
	// billing
	// - accounts -> management/organization
	// web:
	// - frontend -> gateway
	// - gateway  -> management/organization
	// - gateway  -> billing/accounts

	organization := "management/organization"
	accounts := "billing/accounts"
	gateway := "web/gateway"
	frontend := "web/frontend"

	assert.Equal(t, 3, len(project.Applications))

	dep, err := architecture.NewServiceDependencies(ctx, project)
	assert.NoError(t, err)
	assert.NotNil(t, dep)

	assert.Equal(t, 4, len(dep.Services()))

	assert.Equal(t, 4, len(dep.Dependencies()))

	// Sanity checks
	ok, err := dep.DependsOn(accounts, organization)
	assert.NoError(t, err)
	assert.True(t, ok)

	ok, err = dep.DependsOn(gateway, organization)
	assert.NoError(t, err)
	assert.True(t, ok)

	ok, err = dep.DependsOn(gateway, accounts)
	assert.NoError(t, err)
	assert.True(t, ok)

	ok, err = dep.DependsOn(frontend, organization)
	assert.NoError(t, err)
	assert.True(t, ok)

	// Check Restrict
	smallerDep, err := dep.Restrict(ctx, accounts)
	assert.NoError(t, err)
	assert.ElementsMatch(t, createServices(accounts, organization), smallerDep.Services())

	// Check DirectRequires

	deps, err := dep.DirectRequires(ctx, organization)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(deps))

	deps, err = dep.DirectRequires(ctx, accounts)
	assert.NoError(t, err)
	assert.Equal(t, createServices(organization), deps)

	deps, err = dep.DirectRequires(ctx, gateway)
	assert.NoError(t, err)
	assert.Equal(t, createServices(organization, accounts), deps)

	deps, err = dep.DirectRequires(ctx, frontend)
	assert.NoError(t, err)
	assert.Equal(t, createServices(gateway), deps)

	// Check DirectDependents

	reqs, err := dep.DirectDependents(ctx, frontend)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(reqs))

	reqs, err = dep.DirectDependents(ctx, gateway)
	assert.NoError(t, err)
	assert.Equal(t, createServices(frontend), reqs)

	reqs, err = dep.DirectDependents(ctx, organization)
	assert.NoError(t, err)
	assert.Equal(t, createServices(accounts, gateway), reqs)

	// Topological sorts

	order, err := dep.OrderTo(ctx, organization)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(order))

	order, err = dep.OrderTo(ctx, accounts)
	assert.NoError(t, err)
	expected := []architecture.Service{
		{organization},
	}
	assert.Equal(t, expected, order)

	order, err = dep.OrderTo(ctx, gateway)
	assert.NoError(t, err)
	expected = []architecture.Service{
		{organization},
		{accounts},
	}
	assert.Equal(t, expected, order)

	order, err = dep.OrderTo(ctx, frontend)
	assert.NoError(t, err)
	expected = []architecture.Service{
		{organization},
		{accounts},
		{gateway},
	}
	assert.Equal(t, expected, order)

}
