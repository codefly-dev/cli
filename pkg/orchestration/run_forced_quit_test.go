package orchestration

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/stretchr/testify/require"
)

// startAgent is a process this test spawned standing in for an agent of the
// run: a child of this process that ends on SIGTERM, as an agent does once it
// has stopped what it runs. It answers the channel closed when it has exited.
func startAgent(t *testing.T, run *exitedRun) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	agent := exec.Command("sleep", "60")
	require.NoError(t, agent.Start())
	exited := make(chan struct{})
	go func() {
		_ = agent.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = agent.Process.Kill()
		<-exited
	})
	run.flow.world.agents.add(agent.Process.Pid)
	return agent, exited
}

// A forced quit abandons the stop that was running. For a disposable run it
// must not abandon the run: nothing runs its scope again, so nothing else
// ever stops its agents or takes back what they left. An agent built before
// agents watched their launcher went on for good under init, with its
// container beside it. The forced quit asks the run's own agents to stop,
// and then takes back what the run left, as its ordinary end does.
func TestAForcedQuitOfADisposableRunStopsItsAgentsAndTakesBackWhatItLeft(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	run := newExitedRun(t, started)
	workspace := t.TempDir()
	run.flow.world.Workspace.WithDir(workspace)
	run.flow.world.Env = &environments.Environment{Name: "local"}
	scope := run.flow.WithIsolatedInvocation()
	nativeStateUnder(t, t.TempDir())
	_, first := startAgent(t, run)
	_, second := startAgent(t, run)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reclaim, err := run.flow.TakeBackOnForcedQuit(ctx, started)
	require.NoError(t, err)

	for _, exited := range []<-chan struct{}{first, second} {
		select {
		case <-exited:
		default:
			t.Fatal("an agent of the disposable run was left running by the forced quit")
		}
	}
	require.Equal(t, []runstate.Run{{Workspace: workspace, NamingScope: scope, Disposable: true}}, run.disposed)
	require.Equal(t, []string{"gateway", "minio"}, reclaim.DisposableContainers)
	require.Equal(t, []int{700}, reclaim.ProcessGroups, "the server group its agent left")
}

// A forced quit of a developer's run stays what it was: the person asked to
// leave now, and what the run leaves is theirs to come back to.
func TestAForcedQuitOfAPersistentRunTakesNothingBack(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	for name, scope := range map[string]string{"plain": "", "a scope the developer named": "pinned"} {
		t.Run(name, func(t *testing.T) {
			run := newExitedRun(t, started)
			run.flow.world.Workspace.WithDir(t.TempDir())
			run.flow.world.Env = &environments.Environment{Name: "local", NamingScope: scope}
			nativeStateUnder(t, t.TempDir())
			_, exited := startAgent(t, run)

			reclaim, err := run.flow.TakeBackOnForcedQuit(context.Background(), started)
			require.NoError(t, err)
			select {
			case <-exited:
				t.Fatal("the forced quit of a persistent run signalled its agent")
			case <-time.After(300 * time.Millisecond):
			}
			require.Empty(t, run.disposed)
			require.Empty(t, run.recovered)
			require.Empty(t, reclaim.DisposableContainers)
			require.Len(t, run.present, 2)
		})
	}
}

// A process that is not this run's child is never signalled, whatever pid the
// run recorded: a pid is reused once its process is reaped.
func TestAForcedQuitSignalsOnlyTheRunsOwnChildren(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	run := newExitedRun(t, started)
	run.flow.world.Workspace.WithDir(t.TempDir())
	run.flow.world.Env = &environments.Environment{Name: "local"}
	run.flow.WithIsolatedInvocation()
	nativeStateUnder(t, t.TempDir())
	_, exited := startAgent(t, run)
	previous := parentOfProcess
	t.Cleanup(func() { parentOfProcess = previous })
	parentOfProcess = func(context.Context, int) (int, bool) { return 1, true }

	_, err := run.flow.TakeBackOnForcedQuit(context.Background(), started)
	require.NoError(t, err)
	select {
	case <-exited:
		t.Fatal("a process that is not this run's child was signalled")
	case <-time.After(300 * time.Millisecond):
	}
}
