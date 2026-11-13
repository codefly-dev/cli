package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Ctrl-C and a failed start take different paths at the run's exit: both
// take back the orphaned process groups (ReclaimAfterRun, always), and only a
// failure removes the containers its agents created, which a stopped run
// keeps for the next run to reuse.
func TestOnlyAFailedRunRemovesItsContainers(t *testing.T) {
	signalled, interrupt := context.WithCancel(context.Background())
	running := context.Background()
	startFailed := errors.New("service tracing/evidence failed during runtime-run")

	require.True(t, runFailed(running, startFailed), "a start that failed with no signal")
	require.False(t, runFailed(running, nil), "a run that finished")
	interrupt()
	require.False(t, runFailed(signalled, startFailed), "a run stopped with Ctrl-C, whatever its teardown returned")

	docker := []string{resources.RuntimeContextFree}
	require.True(t, removesContainers(runFailed(running, startFailed), docker))
	require.False(t, removesContainers(runFailed(signalled, startFailed), docker))
	require.False(t, removesContainers(true, []string{resources.RuntimeContextNative}), "an explicit native run never reaches for Docker")
}

func TestReclaimRunWithoutAFlowIsANoop(t *testing.T) {
	require.NoError(t, reclaimRun(context.Background(), nil, time.Time{}, true))
}
