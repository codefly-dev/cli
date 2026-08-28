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

// Digest capture is opt-in per flow and defaults off. A `build module` flow
// never consumes a digest, so it must not opt in and must not be pulled into
// (or fail on) digest resolution just because a sibling `build service` flow
// shares the process and asked for it.
func TestWithImageDigestDefaultsOffAndIsScopedPerFlow(t *testing.T) {
	service := &Flow{world: &World{Mode: BuildMode}}
	module := &Flow{world: &World{Mode: BuildMode}}

	require.False(t, service.world.CaptureImageDigest, "a fresh flow must default to no digest capture")

	service.WithImageDigest(true)

	require.True(t, service.world.CaptureImageDigest)
	require.False(t, module.world.CaptureImageDigest, "opting one flow into digest capture must not affect another")
}
