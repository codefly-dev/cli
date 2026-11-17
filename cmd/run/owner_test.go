package run

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/codefly-dev/core/runners/runstate"
	"github.com/stretchr/testify/require"
)

// startOwner is a process standing in for whatever started a run for itself:
// a test binary driving a dependency stack through the SDK.
func startOwner(t *testing.T) (*exec.Cmd, runstate.Birth) {
	t.Helper()
	// Bounded, so that a failed test cannot leave it behind for good.
	owner := exec.Command("sleep", "60")
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	birth, err := runstate.BirthOf(owner.Process.Pid)
	require.NoError(t, err)
	return owner, birth
}

// A run started for a process is that process's: when it is gone, however it
// went, the run stops in order as if it had been asked to. A test binary that
// hit its timeout or was killed never calls stop, and the stack used to
// outlive it for good.
func TestARunStopsWhenTheProcessItWasStartedForIsGone(t *testing.T) {
	owner, birth := startOwner(t)
	ctx, release := stopWithOwner(context.Background(), birth)
	defer release()

	select {
	case <-ctx.Done():
		t.Fatal("the run stopped while its owner still runs")
	case <-time.After(time.Second):
	}

	require.NoError(t, owner.Process.Kill())
	_ = owner.Wait()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the owner was killed and the run did not stop")
	}
}

// The declaration is read once and is this run's alone: the agents and
// services the run starts are the run's, not its owner's, and must not take
// themselves for runs that end with that process.
func TestARunTakesItsOwnerOutOfItsEnvironment(t *testing.T) {
	_, birth := startOwner(t)
	t.Setenv(runstate.OwnerEnvironment, birth.String())

	owner, owned, err := takeOwner()
	require.NoError(t, err)
	require.True(t, owned)
	require.Equal(t, birth, owner)
	_, owned, err = runstate.Owner(os.Environ())
	require.NoError(t, err)
	require.False(t, owned, "the declaration was left for the processes this run starts")

	_, owned, err = takeOwner()
	require.NoError(t, err)
	require.False(t, owned, "a run nobody started for itself has no owner")
}

// A declaration that cannot be read is refused: started anyway, the run
// would outlive the process that said it owns it.
func TestARunRefusesAnOwnerItCannotRead(t *testing.T) {
	t.Setenv(runstate.OwnerEnvironment, "not-a-birth")
	_, _, err := takeOwner()
	require.ErrorContains(t, err, runstate.OwnerEnvironment)
}
