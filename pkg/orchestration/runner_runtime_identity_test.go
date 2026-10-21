package orchestration

import (
	"fmt"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func TestRunnerRuntimeOverridesCarryAuthoritativeNamingScope(t *testing.T) {
	input := map[string]string{
		"APPLICATION_MODE":          "dogfood",
		resources.NamingScopePrefix: "caller-must-not-override-runtime-identity",
	}
	runner := &Runner{
		world:     &World{Env: &environments.Environment{NamingScope: "stable"}},
		overrides: input,
	}

	got := testRuntimeOverrides(t, runner)

	require.Equal(t, "dogfood", got["APPLICATION_MODE"])
	require.Equal(t, "stable", got[resources.NamingScopePrefix])
	require.Equal(
		t,
		"caller-must-not-override-runtime-identity",
		input[resources.NamingScopePrefix],
		"deriving runtime identity must not mutate caller-owned overrides",
	)
}

func TestRunnerRuntimeOverridesOmitUnselectedNamingScope(t *testing.T) {
	runner := &Runner{world: &World{Env: &environments.Environment{}}}
	require.Empty(t, testRuntimeOverrides(t, runner))
}

func testRuntimeOverrides(t *testing.T, runner *Runner) map[string]string {
	t.Helper()
	got, err := runner.runtimeOverrides()
	require.NoError(t, err)
	return got
}
func testRuntimeOverridesFor(t *testing.T, runner *Runner, own []*basev0.NetworkMapping) map[string]string {
	t.Helper()
	got, err := runner.runtimeOverridesFor(own)
	require.NoError(t, err)
	return got
}

func TestRuntimeProjectionRefusalAndConflictingOverride(t *testing.T) {
	service := &resources.Service{Name: "api"}
	runner := &Runner{instance: &services.Instance{Service: service}, world: &World{
		RuntimeOverrides: func(*resources.Service) (map[string]string, error) { return nil, fmt.Errorf("projection unavailable") },
	}}
	_, err := runner.runtimeOverrides()
	require.ErrorContains(t, err, "projection unavailable")
	runner.world.RuntimeOverrides = func(*resources.Service) (map[string]string, error) {
		return map[string]string{environments.NativeIdentityCredentials: `{"second":"/generated"}`}, nil
	}
	runner.overrides = map[string]string{environments.NativeIdentityCredentials: `{"second":"/authored"}`}
	_, err = runner.runtimeOverrides()
	require.ErrorContains(t, err, "conflicts")
}

func TestNativeIdentityRefusesPerServiceContainerSelection(t *testing.T) {
	runner := &Runner{instance: &services.Instance{Service: &resources.Service{Name: "api"}}, runtimeContext: resources.RuntimeContextContainer,
		world: &World{Env: &environments.Environment{Host: &environments.EnvironmentHost{}, NativeIdentity: &environments.EnvironmentNativeIdentity{Verifiers: []string{"example/api"}}},
			RuntimeOverrides: func(*resources.Service) (map[string]string, error) {
				return map[string]string{"CODEFLY_NATIVE_IDENTITY_ANCHOR": "/run/anchor"}, nil
			},
		},
	}
	_, err := runner.runtimeOverrides()
	require.ErrorContains(t, err, "native-identity requires")
	runner.world.RuntimeOverrides = func(*resources.Service) (map[string]string, error) { return nil, nil }
	_, err = runner.runtimeOverrides()
	require.NoError(t, err, "an unrelated container service receives no native identity")
}
