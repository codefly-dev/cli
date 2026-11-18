package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/internal/killedrun"
	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// endedRun is the native state directory of a run that is no longer alive.
func endedRun(t *testing.T, root string, run runstate.Run, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{root}, parts...)...)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pgdata"), 0o700))
	hold, err := runstate.Claim(dir, run)
	require.NoError(t, err)
	require.NoError(t, hold.Release())
	return dir
}

// `codefly clear` removed processes and containers only, so the native
// database directories of disposable runs that were killed stayed on the
// machine for good. It removes them, in its own scope, and never a
// developer's persistent database.
func TestClearRemovesNativeStateOfEndedDisposableRuns(t *testing.T) {
	root := t.TempDir()
	here, elsewhere := t.TempDir(), t.TempDir()
	w := wool.Get(context.Background()).In("clear")

	mine := endedRun(t, root, runstate.Run{Workspace: here, NamingScope: "invaaaaaaaa", Disposable: true}, "postgres", "run", "1111")
	theirs := endedRun(t, root, runstate.Run{Workspace: elsewhere, NamingScope: "invbbbbbbbb", Disposable: true}, "redis", "2222")
	persistent := endedRun(t, root, runstate.Run{Workspace: here}, "postgres", "run", "3333")

	scoped := clearOptions{verb: "clear", scope: processgroup.InWorkspace(here)}

	preview := scoped
	preview.dryRun = true
	require.Empty(t, sweepNativeRunState(w, root, preview))
	require.DirExists(t, mine, "a dry run removes nothing")

	require.Empty(t, sweepNativeRunState(w, root, scoped))
	require.NoDirExists(t, mine)
	require.DirExists(t, theirs, "another workspace's state is out of scope")
	require.DirExists(t, persistent)

	require.Empty(t, sweepNativeRunState(w, root, clearOptions{verb: "clear", scope: processgroup.AllWorkspaces()}))
	require.NoDirExists(t, theirs)
	require.DirExists(t, persistent, "a developer's database is never removed")
}

func TestKilledRunHelper(_ *testing.T) { killedrun.Helper() }

// A killed run leaves its servers running too, and a directory is in use for
// as long as one runs: `codefly clear` kept the directory, and the server,
// for good. It stops the servers a killed disposable run left in its scope and
// then removes the directory; a preview names them and stops nothing; and
// --keep-processes, another workspace's run and a developer's database keep
// theirs.
func TestClearStopsTheServersAKilledDisposableRunLeft(t *testing.T) {
	root := t.TempDir()
	here, elsewhere := t.TempDir(), t.TempDir()
	w := wool.Get(context.Background()).In("clear")

	mine := filepath.Join(root, "clickhouse", "1111")
	mineServer := killedrun.Leave(t, mine, runstate.Run{Workspace: here, NamingScope: "invaaaaaaaa", Disposable: true})
	theirs := filepath.Join(root, "clickhouse", "2222")
	theirServer := killedrun.Leave(t, theirs, runstate.Run{Workspace: elsewhere, NamingScope: "invbbbbbbbb", Disposable: true})
	persistent := filepath.Join(root, "postgres", "run", "3333")
	persistentServer := killedrun.Leave(t, persistent, runstate.Run{Workspace: here})

	scoped := clearOptions{verb: "clear", scope: processgroup.InWorkspace(here)}

	preview := scoped
	preview.dryRun = true
	require.Empty(t, sweepNativeRunState(w, root, preview))
	require.True(t, mineServer.Running(), "a dry run stops nothing")
	require.DirExists(t, mine)

	keeping := scoped
	keeping.keepProcesses = true
	require.Empty(t, sweepNativeRunState(w, root, keeping))
	require.True(t, mineServer.Running(), "--keep-processes stops nothing")
	require.DirExists(t, mine)

	require.Empty(t, sweepNativeRunState(w, root, scoped))
	mineServer.RequireStopped(t)
	require.NoDirExists(t, mine)
	require.True(t, theirServer.Running(), "another workspace's run is out of scope")
	require.DirExists(t, theirs)
	require.True(t, persistentServer.Running(), "a developer's database server is theirs to stop")
	require.DirExists(t, persistent)
}
