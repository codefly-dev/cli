package orchestration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func TestContainerRecoveryRequiresAgentAcknowledgementBeforeDockerInit(t *testing.T) {
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	scope, err := dockerrun.NewContainerRecoveryScope(t.TempDir(), t.TempDir(), "test")
	require.NoError(t, err)
	require.NoError(t, dockerrun.SetContainerRecoveryScope(scope))
	for _, tc := range []struct {
		name, runtime, ack string
		reject             bool
	}{
		{"legacy Docker agent", resources.RuntimeContextContainer, "", true},
		{"legacy free agent", resources.RuntimeContextFree, "", true},
		{"wrong scope", resources.RuntimeContextContainer, "foreign", true},
		{"acknowledged", resources.RuntimeContextContainer, dockerrun.InheritedContainerRecoveryScope(), false},
		{"native", resources.RuntimeContextNative, "", false},
		{"nix", resources.RuntimeContextNix, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &Runner{runtimeContext: tc.runtime, containerRecoveryIdentity: dockerrun.InheritedContainerRecoveryScope(), instance: &services.Instance{Identity: &resources.ServiceIdentity{Name: "db", Module: "infra"}, ContainerRecoveryScope: tc.ack}}
			if tc.reject {
				// No Runtime client or World is installed: the real Init must
				// return before configuration, port allocation or any agent RPC.
				_, err := runner.Init(t.Context())
				require.ErrorContains(t, err, "did not acknowledge this run's container recovery scope")
			} else {
				require.NoError(t, runner.validateContainerRecovery())
			}
		})
	}
	// A host that could not resolve ownership projects nothing, and the flow
	// warns and runs on. There is no identity to hold an agent to, so the guard
	// steps aside rather than rejecting every agent on such a host.
	t.Run("flow projected nothing", func(t *testing.T) {
		runner := &Runner{runtimeContext: resources.RuntimeContextContainer, instance: &services.Instance{Identity: &resources.ServiceIdentity{Name: "db", Module: "infra"}}}
		require.NoError(t, runner.validateContainerRecovery())
	})
}

// newProjectionWorkspaceFlow builds a real flow whose origin is excluded and
// whose dependency graph is cut, so InitManagers runs its whole sequence
// without spawning an agent process.
func newProjectionWorkspaceFlow(t *testing.T, mode Mode, spawnOrigin bool) (*Flow, *resources.Workspace) {
	t.Helper()
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: recovery
layout: modules
modules:
    - name: web
environments:
    - name: local
      naming-scope: from-yaml
`,
		"modules/web/module.codefly.yaml": `kind: module
name: web
project: recovery
services:
    - name: gateway
`,
		"modules/web/services/gateway/service.codefly.yaml": `kind: service
name: gateway
version: 0.0.0
module: web
agent:
    kind: runtime::service
    name: krakend
    version: 0.0.6
    publisher: codefly.ai
`,
	})
	module, err := workspace.LoadModuleFromName(ctx, "web")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "gateway")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	flow, err := NewFlow(ctx, workspace, module, service, env, mode)
	require.NoError(t, err)
	flow.WithStandAlone(true)
	flow.WithExcludeRoot(!spawnOrigin)
	return flow, workspace
}

// Ownership is projected by initializing managers at all, so every entry point
// that spawns agents through a flow — build, test, ci, deploy, sync,
// validation, gitops and the control plane, not only run — labels the
// containers its agents create.
func TestEveryFlowProjectsContainerRecoveryBeforeSpawningAgents(t *testing.T) {
	for _, mode := range []Mode{RunMode, BuildMode, TestMode, SyncMode, DeployMode, SnapshotMode} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
			t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
			flow, workspace := newProjectionWorkspaceFlow(t, mode, false)

			require.NoError(t, flow.InitManagers(context.Background()))

			// Read the environment before asking the flow for its scope:
			// ContainerRecoveryScope projects on demand, so consulting it first
			// would pass whether or not InitManagers ever projected.
			require.NotEmpty(t, dockerrun.InheritedContainerRecoveryScope())

			expected, err := dockerrun.NewContainerRecoveryScope(resources.CodeflyHomeDir(), workspace.Dir(), "from-yaml")
			require.NoError(t, err)
			scope, err := flow.ContainerRecoveryScope()
			require.NoError(t, err)
			require.Equal(t, expected, scope)
		})
	}
}

// The sweep resolves ownership before InitManagers does, and an agent already
// holding the inherited marker must keep acknowledging the same identity.
func TestContainerRecoveryScopeIsResolvedOnce(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	flow, _ := newProjectionWorkspaceFlow(t, RunMode, false)

	first, err := flow.ContainerRecoveryScope()
	require.NoError(t, err)
	marker := os.Getenv(dockerrun.ContainerRecoveryScopeEnvironment)
	require.NotEmpty(t, marker)

	require.NoError(t, flow.InitManagers(context.Background()))

	second, err := flow.ContainerRecoveryScope()
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, marker, os.Getenv(dockerrun.ContainerRecoveryScopeEnvironment))
}

// The ordering is the whole guarantee: New() spawns the agent process, which
// inherits the marker at exec. Let the spawn actually be attempted (and fail on
// an agent this fixture never installs) and require the marker to already be
// projected — moving the projection after the spawn loop fails this.
func TestContainerRecoveryIsProjectedBeforeTheSpawnLoop(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	flow, _ := newProjectionWorkspaceFlow(t, RunMode, true)

	require.Error(t, flow.InitManagers(context.Background()), "fixture agent must not resolve, so the spawn is attempted and fails")
	require.NotEmpty(t, dockerrun.InheritedContainerRecoveryScope(), "ownership must be projected before the spawn loop runs")
}

// A second flow in this process projects its own identity over the variable.
// The first flow's agents still hold what they inherited at spawn — and a
// cached agent holds what some earlier flow's spawn gave it — so validating
// against the live variable reports a correctly rebuilt agent as stale.
func TestContainerRecoveryValidatesAgainstTheFlowsOwnProjection(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	flow, _ := newProjectionWorkspaceFlow(t, RunMode, false)
	require.NoError(t, flow.InitManagers(context.Background()))
	projected := flow.containerRecoveryIdentity
	require.NotEmpty(t, projected)

	// Every runner this flow builds carries that identity.
	wired := &Runner{}
	flow.configureRunner(wired, flow.originService)
	require.Equal(t, projected, wired.containerRecoveryIdentity)

	agent := &Runner{
		runtimeContext:            resources.RuntimeContextContainer,
		containerRecoveryIdentity: projected,
		instance:                  &services.Instance{Identity: &resources.ServiceIdentity{Name: "db", Module: "infra"}, ContainerRecoveryScope: projected},
	}
	require.NoError(t, agent.validateContainerRecovery())

	// A concurrent flow over a different workspace projects its own ownership.
	other, err := dockerrun.NewContainerRecoveryScope(resources.CodeflyHomeDir(), t.TempDir(), "concurrent-flow")
	require.NoError(t, err)
	require.NoError(t, dockerrun.SetContainerRecoveryScope(other))
	require.NotEqual(t, projected, dockerrun.InheritedContainerRecoveryScope())

	require.NoError(t, agent.validateContainerRecovery(), "the clobbered variable must not fail a correctly acknowledged agent")

	// An agent holding the other flow's ownership is still refused.
	foreign := &Runner{
		runtimeContext:            resources.RuntimeContextContainer,
		containerRecoveryIdentity: projected,
		instance:                  &services.Instance{Identity: &resources.ServiceIdentity{Name: "db", Module: "infra"}, ContainerRecoveryScope: dockerrun.InheritedContainerRecoveryScope()},
	}
	require.ErrorContains(t, foreign.validateContainerRecovery(), "did not acknowledge this run's container recovery scope")
}

// A host that cannot resolve ownership — here an unwritable codefly home — must
// still run. Core degrades the same condition rather than stopping every
// containerized run; failing would take out build, test, ci, deploy, sync,
// gitops and the control plane on hosts where they work today.
func TestInitManagersRunsWhenOwnershipCannotBeResolved(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "home-is-a-file")
	require.NoError(t, os.WriteFile(blocked, []byte("not a directory"), 0o600))
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	flow, _ := newProjectionWorkspaceFlow(t, BuildMode, false)

	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(blocked, "nested"))
	require.NoError(t, flow.InitManagers(context.Background()))
	require.Empty(t, dockerrun.InheritedContainerRecoveryScope())
	require.Empty(t, flow.containerRecoveryIdentity)
}

// containerRecoveryDigests returns the scope and namespace digests this host
// resolves. They are taken from a real projection so the markers below carry
// digests the parser accepts, and cannot drift into fixtures it rejects for
// being malformed rather than for the generation they came from.
func containerRecoveryDigests(t *testing.T) (string, string) {
	t.Helper()
	scope, err := dockerrun.NewContainerRecoveryScope(t.TempDir(), t.TempDir(), "mixed-generation")
	require.NoError(t, err)
	require.NoError(t, dockerrun.SetContainerRecoveryScope(scope))
	identity := dockerrun.InheritedContainerRecoveryScope()
	require.NotEmpty(t, identity)
	id, namespace, ok := strings.Cut(identity, ":")
	require.True(t, ok)
	return id, namespace
}

// unownedPID is a pid no live process holds. Deriving one from os.Getpid()
// reads as safer and is not: the parser compares against the parent Core
// captured when it initialized, and os.Getppid() stops matching that the moment
// this process is reparented — so a derived neighbour can collide with the
// retained parent and have its marker accepted.
const unownedPID = 999999999

// The rollout and the fleet runbook tell the operator how each generation's
// marker is read, and this CLI's own Core is what reads it. Core owns that
// behavior and tests it directly, including the container creation these
// resolutions feed — which is unreachable from here, because
// desiredContainerConfigs is unexported and NewDockerEnvironment pings a
// daemon. What this pins is the CLI's documented dependency on that behavior: a
// Core bump changing any row below would leave the release guidance wrong with
// every test in this repository still green.
//
// The rollout's claim that the two unsupported directions "fail differently" is
// true only at creation. The identity resolved here is the whole of what an
// agent echoes as its acknowledgement and all Runner.Init's guard compares, and
// a refused marker and a missing one both resolve to nothing — so the guard
// catches neither direction.
func TestPinnedCoreMarkerResolutionsMatchTheRollout(t *testing.T) {
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	id, namespace := containerRecoveryDigests(t)
	// A host that cannot prove a durable identity projects the exact scope
	// alone, so the namespace may be empty here. An untagged marker needs some
	// trailing field to be ambiguous at all, and any valid digest carries the
	// ambiguity this generation refuses to guess at.
	trailing := namespace
	if trailing == "" {
		trailing = id
	}
	for _, tc := range []struct{ name, marker, identity string }{
		{"a released CLI projects nothing", "", ""},
		{"the untagged generation, ambiguous trailing field", fmt.Sprintf("%d:%s:%s", os.Getpid(), id, trailing), ""},
		{"the untagged generation, exact scope alone", fmt.Sprintf("%d:%s", os.Getpid(), id), id + ":"},
		{"neither this process nor its parent", fmt.Sprintf("%d:v2:%s:%s", unownedPID, id, trailing), ""},
		{"this generation", fmt.Sprintf("%d:v2:%s:%s", os.Getpid(), id, namespace), id + ":" + namespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, tc.marker)
			require.Equal(t, tc.identity, dockerrun.InheritedContainerRecoveryScope())
		})
	}
}

// A CLI of this generation can itself be launched under a marker another one
// left behind — a nested run, the re-exec'd daemon, an agent shelling out. The
// flow has to project its own ownership over whatever it inherited, and the two
// shapes it can inherit fail differently if it does not. A marker this
// generation refuses leaves its agents creating no containers at all. One that
// parses is worse and quieter: the agents label containers with another owner's
// identity, which no sweep of this flow can ever match, so the leak this
// recovery exists to collect resumes silently.
func TestFlowProjectsOverAnInheritedForeignMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tagged bool
	}{
		{"a marker this generation refuses", false},
		{"another flow's well-formed marker", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Create the home directory here rather than relying on
			// InitManagers to do it: resolving the expected ownership below
			// walks that path, so a flow that never projected would fail this
			// on a missing directory instead of on the ownership it projected.
			home := filepath.Join(t.TempDir(), "home")
			require.NoError(t, os.MkdirAll(home, 0o700))
			t.Setenv(resources.CodeflyHomeEnv, home)
			t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
			id, namespace := containerRecoveryDigests(t)
			trailing := namespace
			if trailing == "" {
				trailing = id
			}
			inherited := fmt.Sprintf("%d:%s:%s", os.Getpid(), id, trailing)
			if tc.tagged {
				inherited = fmt.Sprintf("%d:v2:%s:%s", os.Getpid(), id, namespace)
			}
			t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, inherited)
			foreign := dockerrun.InheritedContainerRecoveryScope()
			require.Equal(t, tc.tagged, foreign != "", "the fixture must be a marker this generation reads as expected")

			flow, workspace := newProjectionWorkspaceFlow(t, RunMode, false)
			require.NoError(t, flow.InitManagers(context.Background()))

			expected, err := dockerrun.NewContainerRecoveryScope(resources.CodeflyHomeDir(), workspace.Dir(), "from-yaml")
			require.NoError(t, err)
			scope, err := flow.ContainerRecoveryScope()
			require.NoError(t, err)
			require.Equal(t, expected, scope)
			projected := dockerrun.InheritedContainerRecoveryScope()
			require.NotEmpty(t, projected)
			require.NotEqual(t, foreign, projected, "the flow must project its own ownership, not adopt what it inherited")
		})
	}
}

// A build-mode Manager has a Builder and no Runner, and InitManagers still
// configures it. Projecting the recovery identity onto that nil runner is
// what took `codefly ci run --phase build` down with a nil dereference in
// 0.1.151; the setters it sits beside already tolerate the nil.
func TestConfigureRunnerToleratesTheBuildModeNilRunner(t *testing.T) {
	flow := &Flow{world: &World{Mode: BuildMode}, containerRecoveryIdentity: "scope"}
	service := &resources.Service{Name: "accounts"}
	require.NotPanics(t, func() { flow.configureRunner(nil, service) })

	runner := &Runner{}
	flow.configureRunner(runner, service)
	require.Equal(t, "scope", runner.containerRecoveryIdentity)
}
