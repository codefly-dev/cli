package deploy

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A composition renders the host's module first, then the rest by name, and
// leaves out every module the environment withdraws whole: a module with one
// service still composed renders as before.
func TestCompositionRenderOrderLeavesOutAWithdrawnModule(t *testing.T) {
	module := func(name string, services ...string) *resources.Module {
		m := &resources.Module{Name: name}
		for _, service := range services {
			m.ServiceReferences = append(m.ServiceReferences, &resources.ServiceReference{Name: service})
		}
		return m
	}
	env := &environments.Environment{
		Name: "staging",
		Host: &environments.EnvironmentHost{Delivery: "saas/accounts/connect"},
		ExcludeServices: []environments.EnvironmentExcludedService{
			{Service: "authority/local"}, {Service: "authority/hop"},
			{Service: "runtime/witness"},
		},
	}
	modules := []*resources.Module{
		module("runtime", "tasks", "witness"),
		module("authority", "local", "hop"),
		module("documents", "documents"),
		module("saas", "accounts"),
	}

	rendered, withdrawn := compositionRenderOrder(env, modules)

	var names []string
	for _, m := range rendered {
		names = append(names, m.Name)
	}
	require.Equal(t, []string{"saas", "documents", "runtime"}, names)
	require.Equal(t, []string{"authority"}, withdrawn)
}
