//go:build integration

package orchestration

import (
	"context"
	"debug/buildinfo"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// AgentBinaryEnv names the agent under qualification. It is a binary rather
// than a fixed release pin so CI can rebuild against the Core this CLI pins.
// Release qualification can also supply a downloaded published binary.
const AgentBinaryEnv = "CODEFLY_CONTAINER_RECOVERY_AGENT_BINARY"

// AgentVersionEnv carries the version the binary in AgentBinaryEnv was built
// from. It is supplied rather than written here because the version is only a
// cache-path component: a literal would keep passing after the qualified source
// moved, labelling one build with another's version.
const AgentVersionEnv = "CODEFLY_CONTAINER_RECOVERY_AGENT_VERSION"

// AgentNameEnv carries the agent the binary implements. The qualification runs
// over every inventory row that reaches a container through a Core companion,
// and each resolves to its own cache path, so a literal here would install one
// row's binary under another row's identity and qualify neither.
const AgentNameEnv = "CODEFLY_CONTAINER_RECOVERY_AGENT_NAME"

// AgentRepositoryEnv carries the repository the binary was built from. The
// identity in AgentNameEnv only selects the cache path the binary is installed
// at, and an agent answers the acknowledgement the same way wherever it sits —
// so without holding the build to a repository, a row qualifies whatever
// binary it was handed rather than the agent it claims to cover.
const AgentRepositoryEnv = "CODEFLY_CONTAINER_RECOVERY_AGENT_REPOSITORY"

// corePin is the Core release this CLI is compiled against, read from its own
// build information so the qualification cannot drift from go.mod.
func corePin(t *testing.T) string {
	t.Helper()
	build, ok := debug.ReadBuildInfo()
	require.True(t, ok)
	for _, dep := range build.Deps {
		if dep.Path == "github.com/codefly-dev/core" {
			return dep.Version
		}
	}
	t.Fatal("this build does not depend on github.com/codefly-dev/core")
	return ""
}

// A companion-row agent reaches Docker only through a Core package, and the
// CLI's acknowledgement guard exempts native and Nix — so on the native path
// nothing fails loudly when an agent misreads the ownership marker, and the
// containers it creates are simply never labelled. Qualify that path directly:
// rebuild the agent on the Core this CLI pins, hand it the identity a native
// run projects, and require it back verbatim.
func TestRebuiltCompanionAgentAcknowledgesNativeContainerRecovery(t *testing.T) {
	source := os.Getenv(AgentBinaryEnv)
	require.NotEmpty(t, source, "rebuild a companion agent on the pinned Core and set "+AgentBinaryEnv)
	version := os.Getenv(AgentVersionEnv)
	require.NotEmpty(t, version, "set "+AgentVersionEnv+" to the version that binary was built from")
	name := os.Getenv(AgentNameEnv)
	require.NotEmpty(t, name, "set "+AgentNameEnv+" to the agent that binary implements")
	repository := os.Getenv(AgentRepositoryEnv)
	require.NotEmpty(t, repository, "set "+AgentRepositoryEnv+" to the repository that binary was built from")

	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv(recoveryscope.EnvironmentVariable, "")

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, name+":"+version)
	require.NoError(t, err)
	path, err := agent.Path(ctx)
	require.NoError(t, err)
	binary, err := os.ReadFile(source)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, binary, 0o700))

	build, err := buildinfo.ReadFile(path)
	require.NoError(t, err)
	// Every assertion below reads the binary, and nothing else establishes
	// which agent that binary is: it was installed under an identity this test
	// was told, and it answers the same way whatever identity that was. Hold
	// it to the repository the caller claims to be qualifying, or a row passes
	// while covering a different agent's build.
	require.Equal(t, "github.com/codefly-dev/"+repository, build.Main.Path,
		"the binary under qualification was built from %s, not the repository this qualification names", build.Main.Path)
	var agentCore string
	for _, dep := range build.Deps {
		if dep.Path == "github.com/codefly-dev/core" {
			agentCore = dep.Version
		}
	}
	// The rollout gate is "6a40c4bf28ac or later", and this CLI's own pin is
	// already past it, so anything at or after that pin understands the marker
	// this CLI writes. Requiring equality would fail a developer who rebuilt
	// the agent on a newer Core, which the gate explicitly allows.
	require.GreaterOrEqual(t, semver.Compare(agentCore, corePin(t)), 0,
		"the agent under qualification embeds Core %s, older than this CLI's %s", agentCore, corePin(t))

	// The identity `codefly run --runtime-context=native` projects.
	scope, err := dockerrun.NewContainerRecoveryScope(resources.CodeflyHomeDir(), t.TempDir(), "native-qualification")
	require.NoError(t, err)
	require.NoError(t, dockerrun.SetContainerRecoveryScope(scope))

	const cacheKey = "container-recovery-native-agent"
	t.Cleanup(func() { services.ClearAgent(cacheKey) })
	client, err := services.LoadAgent(ctx, agent, cacheKey)
	require.NoError(t, err)

	var headers metadata.MD
	_, err = client.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{}, grpc.Header(&headers))
	require.NoError(t, err)
	acknowledgement := strings.Join(headers.Get(recoveryscope.Header), "")
	require.Equal(t, recoveryscope.Acknowledgement(), acknowledgement,
		"the agent must echo the exact identity it will stamp on the containers it creates")

	// The same binary is therefore usable on every selection, not only the two
	// the guard exempts.
	for _, runtimeContext := range []string{
		resources.RuntimeContextNative,
		resources.RuntimeContextNix,
		resources.RuntimeContextContainer,
		resources.RuntimeContextFree,
	} {
		runner := &Runner{
			runtimeContext: runtimeContext,
			// What Flow.configureRunner hands every runner it builds. Without
			// it validateContainerRecovery has no identity to hold the agent
			// to and accepts anything, qualifying nothing.
			containerRecoveryIdentity: recoveryscope.Acknowledgement(),
			instance: &services.Instance{
				Identity:               &resources.ServiceIdentity{Name: "api", Module: "app"},
				ContainerRecoveryScope: acknowledgement,
			},
		}
		require.NoError(t, runner.validateContainerRecovery(), runtimeContext)
	}
}
