package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/cli/internal/killedrun"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/stretchr/testify/require"
)

// stoppedState is the native state directory a run's store agent leaves once
// the run stopped it: the data, and no process using it.
func stoppedState(t *testing.T, root string, run runstate.Run, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{root}, parts...)...)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pgdata"), 0o700))
	hold, err := runstate.Claim(dir, run)
	require.NoError(t, err)
	require.NoError(t, hold.Release())
	return dir
}

func nativeStateUnder(t *testing.T, root string) {
	t.Helper()
	previous := nativeStateRoot
	t.Cleanup(func() { nativeStateRoot = previous })
	nativeStateRoot = func() (string, error) { return root, nil }
}

// A stopped run only stops its agents: no agent is destroyed, so none removes
// its directory, and every `run --temporary-ports` left a database directory
// on the machine for good (1,042 Postgres, 3,328 Redis and 236 ClickHouse
// directories, 109 GB). The run that generated the scope takes its state back
// when it ends, and nobody else's.
func TestADisposableRunsExitRemovesItsNativeState(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	run := newExitedRun(t, started)
	workspace := t.TempDir()
	run.flow.world.Workspace.WithDir(workspace)
	run.flow.world.Env = &environments.Environment{Name: "local"}
	scope := run.flow.WithIsolatedInvocation()
	require.NotEmpty(t, scope)

	root := t.TempDir()
	nativeStateUnder(t, root)
	postgres := stoppedState(t, root, runstate.Run{Workspace: workspace, NamingScope: scope, Disposable: true}, "postgres", "run", "1111")
	redis := stoppedState(t, root, runstate.Run{Workspace: workspace, NamingScope: scope, Disposable: true}, "redis", "2222")
	otherRun := stoppedState(t, root, runstate.Run{Workspace: workspace, NamingScope: "invotherrun", Disposable: true}, "redis", "3333")
	otherWorkspace := stoppedState(t, root, runstate.Run{Workspace: t.TempDir(), NamingScope: scope, Disposable: true}, "redis", "4444")
	developers := stoppedState(t, root, runstate.Run{Workspace: workspace}, "postgres", "run", "5555")

	require.NoError(t, run.flow.Stop())
	reclaim, err := run.flow.ReclaimAfterRun(context.Background(), started, false)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{postgres, redis}, reclaim.NativeState)
	require.NoDirExists(t, postgres)
	require.NoDirExists(t, redis)
	require.DirExists(t, otherRun, "another run's state is that run's, or `codefly clear`'s")
	require.DirExists(t, otherWorkspace)
	require.DirExists(t, developers, "a developer's database is never removed")
}

// A plain run, or one whose scope the caller named, owns nothing disposable.
func TestAPersistentRunsExitRemovesNoNativeState(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	for name, scope := range map[string]string{"plain": "", "named scope": "pinned"} {
		t.Run(name, func(t *testing.T) {
			run := newExitedRun(t, started)
			workspace := t.TempDir()
			run.flow.world.Workspace.WithDir(workspace)
			run.flow.world.Env = &environments.Environment{Name: "local", NamingScope: scope}

			root := t.TempDir()
			nativeStateUnder(t, root)
			// Even a directory wrongly recorded as disposable under this run's
			// scope is not this run's to remove: it never declared it.
			recorded := stoppedState(t, root, runstate.Run{Workspace: workspace, NamingScope: scope, Disposable: true}, "postgres", "run", "1111")

			require.NoError(t, run.flow.Stop())
			reclaim, err := run.flow.ReclaimAfterRun(context.Background(), started, false)
			require.NoError(t, err)
			require.Empty(t, reclaim.NativeState)
			require.DirExists(t, recorded)
		})
	}
}

func TestKilledRunHelper(_ *testing.T) { killedrun.Helper() }

// A server still running in a directory recorded for this run's own scope,
// whose agent and that agent's launcher are both gone, is nobody's but this
// run's to stop: the scope was generated for it and nothing runs it again.
// The exit stops it and removes the directory; the same left under another
// run's scope is that run's, or `codefly clear`'s.
func TestADisposableRunsExitStopsTheServersLeftInItsNativeState(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	run := newExitedRun(t, started)
	workspace := t.TempDir()
	run.flow.world.Workspace.WithDir(workspace)
	run.flow.world.Env = &environments.Environment{Name: "local"}
	scope := run.flow.WithIsolatedInvocation()
	require.NotEmpty(t, scope)

	root := t.TempDir()
	nativeStateUnder(t, root)
	mine := filepath.Join(root, "clickhouse", "1111")
	mineServer := killedrun.Leave(t, mine, runstate.Run{Workspace: workspace, NamingScope: scope, Disposable: true})
	otherRun := filepath.Join(root, "clickhouse", "2222")
	otherServer := killedrun.Leave(t, otherRun, runstate.Run{Workspace: workspace, NamingScope: "invotherrun", Disposable: true})

	require.NoError(t, run.flow.Stop())
	reclaim, err := run.flow.ReclaimAfterRun(context.Background(), started, false)
	require.NoError(t, err)

	require.Equal(t, []string{mine}, reclaim.NativeState)
	mineServer.RequireStopped(t)
	require.NoDirExists(t, mine)
	require.True(t, otherServer.Running(), "another run's server is that run's, or `codefly clear`'s")
	require.DirExists(t, otherRun)
}
