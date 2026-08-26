package orchestration

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Push is carried on each flow's World rather than a process-global. A
// snapshot render requires push to resolve the immutable digest, but that
// decision must not leak into a later BuildMode flow that shares the process
// (e.g. a control-plane build that refused push).
func TestWithPushIsScopedPerFlow(t *testing.T) {
	snapshot := &Flow{world: &World{Mode: SnapshotMode}}
	build := &Flow{world: &World{Mode: BuildMode}}

	require.False(t, build.world.Push, "a fresh flow must default to no-push")

	snapshot.WithPush(true)

	require.True(t, snapshot.world.Push)
	require.False(t, build.world.Push, "enabling push on one flow must not affect another")
}
