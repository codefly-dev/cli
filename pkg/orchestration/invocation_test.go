package orchestration

import (
	"regexp"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Container names, nix store paths and log roots are all derived from the
// naming scope, so an invocation ID has to survive every one of them.
var invocationIDShape = regexp.MustCompile(`^inv[a-z2-7]{8}$`)

func TestNewInvocationIDIsUnpredictableAndNameSafe(t *testing.T) {
	seen := make(map[string]bool, 128)
	for range 128 {
		id := NewInvocationID()
		require.Regexp(t, invocationIDShape, id)
		require.False(t, seen[id], "NewInvocationID repeated %q", id)
		seen[id] = true
	}
}

// Two disposable flows over the same workspace and environment are exactly the
// audited collision: independent test packages, or two worktrees of one
// workspace, each running `run service --temporary-ports` with no scope flag.
func TestIsolatedInvocationGivesEachFlowItsOwnScope(t *testing.T) {
	first := newFlowForEnvironment(&resources.Environment{})
	second := newFlowForEnvironment(&resources.Environment{})

	firstID := first.WithIsolatedInvocation()
	secondID := second.WithIsolatedInvocation()

	require.Regexp(t, invocationIDShape, firstID)
	require.Regexp(t, invocationIDShape, secondID)
	require.NotEqual(t, firstID, secondID)
	require.Equal(t, firstID, first.world.Env.NamingScope)
	require.Equal(t, secondID, second.world.Env.NamingScope)
}

// The generated identity has to reach the agents, or containers and state
// directories stay shared however unique the ID is.
func TestIsolatedInvocationPropagatesToRunners(t *testing.T) {
	flow := newFlowForEnvironment(&resources.Environment{})
	id := flow.WithIsolatedInvocation()
	runner := &Runner{world: flow.world}

	require.Equal(t, id, runner.runtimeOverrides()[resources.NamingScopePrefix])
}

// The scope is a human label the caller owns; codefly must not silently
// rename the resources that caller asked for.
func TestIsolatedInvocationKeepsAnExplicitNamingScope(t *testing.T) {
	flow := newFlowForEnvironment(&resources.Environment{NamingScope: "pinned"})

	require.Empty(t, flow.WithIsolatedInvocation())
	require.Equal(t, "pinned", flow.world.Env.NamingScope)
}

// Temporary ports are a port strategy shared with `codefly ci run`, whose
// conformance workspace has always used unscoped resource names. Only a caller
// that knows its resources are throwaway may ask for an identity, so asking
// for ephemeral ports must not quietly rename anything.
func TestTemporaryPortsAloneDoesNotRenameResources(t *testing.T) {
	flow := newFlowForEnvironment(&resources.Environment{})

	flow.WithTemporaryPorts(true)

	require.Empty(t, flow.world.Env.NamingScope)
}

// Stable development reuse is the whole point of a named port: `codefly run
// service` must find the same containers and state it left behind.
func TestStableRunKeepsItsIdentity(t *testing.T) {
	flow := newFlowForEnvironment(&resources.Environment{})
	flow.WithTemporaryPorts(false)

	require.Empty(t, flow.world.Env.NamingScope)
}

func newFlowForEnvironment(env *resources.Environment) *Flow {
	return &Flow{world: &World{Env: env, OutputSink: noopOutputSink{}}}
}
