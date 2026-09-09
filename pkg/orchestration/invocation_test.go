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
func TestTemporaryPortsIsolatesEachInvocation(t *testing.T) {
	first := disposableFlow(t)
	second := disposableFlow(t)

	require.Regexp(t, invocationIDShape, first.InvocationID())
	require.Regexp(t, invocationIDShape, second.InvocationID())
	require.NotEqual(t, first.InvocationID(), second.InvocationID())
	require.Equal(t, first.InvocationID(), first.world.Env.NamingScope)
	require.Equal(t, second.InvocationID(), second.world.Env.NamingScope)
}

// The generated identity has to reach the agents, or containers and state
// directories stay shared however unique the ID is.
func TestTemporaryPortsPropagatesInvocationToRunners(t *testing.T) {
	flow := disposableFlow(t)
	runner := &Runner{world: flow.world}

	require.Equal(t, flow.InvocationID(), runner.runtimeOverrides()[resources.NamingScopePrefix])
}

// The scope is a human label the caller owns; codefly must not silently
// rename the resources that caller asked for.
func TestTemporaryPortsKeepsAnExplicitNamingScope(t *testing.T) {
	flow := &Flow{world: &World{Env: &resources.Environment{NamingScope: "pinned"}, OutputSink: noopOutputSink{}}}
	flow.WithTemporaryPorts(true)

	require.Equal(t, "pinned", flow.world.Env.NamingScope)
	require.Empty(t, flow.InvocationID())
}

// Stable development reuse is the whole point of a named port: `codefly run
// service` must find the same containers and state it left behind.
func TestStableRunKeepsItsIdentity(t *testing.T) {
	flow := &Flow{world: &World{Env: &resources.Environment{}, OutputSink: noopOutputSink{}}}
	flow.WithTemporaryPorts(false)

	require.Empty(t, flow.world.Env.NamingScope)
	require.Empty(t, flow.InvocationID())
}

func disposableFlow(t *testing.T) *Flow {
	t.Helper()
	flow := &Flow{world: &World{Env: &resources.Environment{}, OutputSink: noopOutputSink{}}}
	flow.WithTemporaryPorts(true)
	return flow
}
