package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
