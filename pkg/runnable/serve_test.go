package runnable

import (
	"runtime"
	"testing"

	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
)

// The harness reaches nothing: it joins only its unit's internal network,
// publishes nothing, cannot write its root, holds no capability, cannot gain
// privilege, and is bounded in memory and processes. It listens on every
// address of that network, which has no IPv4, so the listen address is IPv6.
func TestTheHarnessContainerHasNoWayOut(t *testing.T) {
	config, host, networking := harnessContainer("codefly.local/runnables/m/tool:0.1.0", "unit-net")
	require.Equal(t, "unit-net", string(host.NetworkMode))
	require.Equal(t, []string{"unit-net"}, keys(networking.EndpointsConfig))
	require.Equal(t, []string{"harness"}, networking.EndpointsConfig["unit-net"].Aliases, "the relay reaches it by a name a DNS label can hold")
	require.Empty(t, host.PortBindings, "the harness publishes nothing; the relay is the only way in")
	require.Empty(t, config.ExposedPorts)
	require.True(t, host.ReadonlyRootfs)
	require.Equal(t, []string{"ALL"}, []string(host.CapDrop))
	require.Empty(t, host.CapAdd)
	require.Contains(t, host.SecurityOpt, "no-new-privileges")
	require.False(t, host.Privileged)
	require.NotZero(t, host.Resources.Memory)
	require.NotNil(t, host.Resources.PidsLimit)
	require.Empty(t, host.Binds, "nothing of the host is mounted: a file reaches the harness as staged bytes")
	require.Empty(t, host.Mounts)
	require.Equal(t, []string{corerunnable.ListenAddressEnv + "=[::]:8000"}, config.Env)
}

// The relay publishes one port on the host's loopback only, runs the pinned
// image unprivileged, and forwards to the harness by name.
func TestTheRelayListensOnLoopbackOnly(t *testing.T) {
	config, host := relayContainer(31337)
	bindings := host.PortBindings[nat.Port("8000/tcp")]
	require.Equal(t, []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "31337"}}, bindings)
	require.Len(t, host.PortBindings, 1)
	require.Equal(t, relayImage, config.Image)
	require.Contains(t, config.Image, "@sha256:", "the bytes between a caller and a harness are pinned")
	require.Equal(t, unprivileged, config.User)
	require.Equal(t, []string{"nc", "-lk", "-p", "8000", "-e", "nc", "harness", "8000"}, []string(config.Cmd))
	require.True(t, host.ReadonlyRootfs)
	require.Equal(t, []string{"ALL"}, []string(host.CapDrop))
}

func TestUnitNameIsTheRunnableInItsScope(t *testing.T) {
	r := &resources.Runnable{Name: "pdf-extract-tables"}
	r.SetModule("robin_nest")
	require.Equal(t, "codefly-runnable-proof-robin-nest-pdf-extract-tables", UnitName("proof", r, ""))
	require.Equal(t, "codefly-runnable-proof-robin-nest-pdf-extract-tables-demo", UnitName("proof", r, "demo"))
}

// A plan is served only when it is one verified image of the runnable's
// repository, built from the context the agent assembled, for this host.
func TestHarnessRecipeAcceptsOnlyTheRunnablesOwnImage(t *testing.T) {
	_, err := harnessRecipe(t.TempDir(), nil, "repo:0.1.0")
	require.ErrorContains(t, err, "without an image recipe")

	recipe := &builderv0.DockerBuildRecipe{
		Name: "runnable", Dockerfile: "Dockerfile", Context: ".", Image: "repo:0.1.0",
		Platforms: []string{"linux/" + runtime.GOARCH}, ContextRoot: builderv0.RecipeContextRoot_RECIPE_CONTEXT_ROOT_OUTPUT,
	}
	_, err = harnessRecipe(t.TempDir(), &builderv0.DockerBuildPlan{Recipes: []*builderv0.DockerBuildRecipe{recipe}}, "repo:0.1.0")
	require.ErrorContains(t, err, "verify the harness image recipe", "an unverified plan is never built")
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
