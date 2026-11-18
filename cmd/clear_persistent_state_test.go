package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// lastClaimed dates a state directory's run records, as if no run had claimed
// it since.
func lastClaimed(t *testing.T, dir string, ago time.Duration) {
	t.Helper()
	when := time.Now().Add(-ago)
	records := filepath.Join(dir, ".codefly-run")
	require.NoError(t, filepath.WalkDir(records, func(path string, _ os.DirEntry, err error) error {
		require.NoError(t, err)
		return os.Chtimes(path, when, when)
	}))
}

// A persistent run's state is a developer's database and is never removed by
// a run or a sweep, so every scope ever stopped left a directory nobody could
// see (178 Postgres ones on one machine). A dry run lists the workspace's,
// each with its age and size, and removes none.
func TestClearListsPersistentRunStateWithAgeAndSize(t *testing.T) {
	root := t.TempDir()
	here, elsewhere := t.TempDir(), t.TempDir()

	mine := endedRun(t, root, runstate.Run{Workspace: here, NamingScope: "feature"}, "postgres", "run", "1111")
	require.NoError(t, os.WriteFile(filepath.Join(mine, "pgdata", "base"), make([]byte, 8192), 0o600))
	lastClaimed(t, mine, 72*time.Hour)
	endedRun(t, root, runstate.Run{Workspace: elsewhere}, "redis", "2222")
	endedRun(t, root, runstate.Run{Workspace: here, NamingScope: "invaaaaaaaa", Disposable: true}, "redis", "3333")
	inUse := filepath.Join(root, "clickhouse", "4444")
	require.NoError(t, os.MkdirAll(inUse, 0o700))
	_, err := runstate.Claim(inUse, runstate.Run{Workspace: here})
	require.NoError(t, err)

	states, err := persistentRunState(root, processgroup.InWorkspace(here))
	require.NoError(t, err)
	require.Len(t, states, 2, "the workspace's persistent state, in use or not, and nothing else")
	require.Equal(t, inUse, states[0].Dir, "newest first")
	require.True(t, states[0].InUse)
	require.Equal(t, mine, states[1].Dir)
	require.Equal(t, "feature", states[1].Run.NamingScope)
	require.False(t, states[1].InUse)
	require.GreaterOrEqual(t, states[1].size, int64(8192))
	require.InDelta(t, 72, time.Since(states[1].LastClaimed).Hours(), 1)

	all, err := persistentRunState(root, processgroup.AllWorkspaces())
	require.NoError(t, err)
	require.Len(t, all, 3)

	w := wool.Get(context.Background()).In("clear")
	require.Empty(t, reportPersistentRunState(w, root, clearOptions{verb: "clear", dryRun: true, scope: processgroup.InWorkspace(here)}))
	require.DirExists(t, mine, "listing removes nothing")
}

// Only what a person names is removed: the named directory, when a run is
// recorded on it, it is this scope's, and nothing uses it.
func TestClearRemovesOnlyThePersistentStateAPersonNames(t *testing.T) {
	root := t.TempDir()
	here, elsewhere := t.TempDir(), t.TempDir()
	w := wool.Get(context.Background()).In("clear")
	scoped := func(dirs ...string) clearOptions {
		return clearOptions{verb: "clear", scope: processgroup.InWorkspace(here), states: dirs}
	}

	named := endedRun(t, root, runstate.Run{Workspace: here, NamingScope: "feature"}, "postgres", "run", "1111")
	kept := endedRun(t, root, runstate.Run{Workspace: here}, "postgres", "run", "2222")
	theirs := endedRun(t, root, runstate.Run{Workspace: elsewhere}, "redis", "3333")
	unrecorded := filepath.Join(root, "postgres", "run", "4444")
	require.NoError(t, os.MkdirAll(filepath.Join(unrecorded, "pgdata"), 0o700))
	inUse := filepath.Join(root, "clickhouse", "5555")
	require.NoError(t, os.MkdirAll(inUse, 0o700))
	_, err := runstate.Claim(inUse, runstate.Run{Workspace: here})
	require.NoError(t, err)

	preview := scoped(named)
	preview.dryRun = true
	require.Empty(t, pruneRunState(w, root, preview))
	require.DirExists(t, named, "a dry run removes nothing")

	require.Empty(t, pruneRunState(w, root, scoped(named)))
	require.NoDirExists(t, named)
	require.DirExists(t, kept, "a directory nobody named stays")

	for reason, dir := range map[string]string{
		"another workspace's state":   theirs,
		"a directory no run records":  unrecorded,
		"state still in use":          inUse,
		"a directory above the state": filepath.Join(root, "postgres"),
	} {
		require.Len(t, pruneRunState(w, root, scoped(dir)), 1, reason)
		require.DirExists(t, dir, reason)
	}
	require.DirExists(t, kept)

	require.Empty(t, pruneRunState(w, root, clearOptions{verb: "clear", scope: processgroup.AllWorkspaces(), states: []string{theirs}}))
	require.NoDirExists(t, theirs, "--all reaches another workspace's named state")
}

// --older-than removes the scope's persistent state no run claimed for that
// long and nothing uses, and nothing else.
func TestClearOlderThanRemovesOnlyOldUnusedPersistentState(t *testing.T) {
	root := t.TempDir()
	here, elsewhere := t.TempDir(), t.TempDir()
	w := wool.Get(context.Background()).In("clear")

	old := endedRun(t, root, runstate.Run{Workspace: here, NamingScope: "feature"}, "postgres", "run", "1111")
	lastClaimed(t, old, 10*24*time.Hour)
	recent := endedRun(t, root, runstate.Run{Workspace: here}, "postgres", "run", "2222")
	lastClaimed(t, recent, 24*time.Hour)
	theirs := endedRun(t, root, runstate.Run{Workspace: elsewhere}, "redis", "3333")
	lastClaimed(t, theirs, 10*24*time.Hour)
	oldInUse := filepath.Join(root, "clickhouse", "4444")
	require.NoError(t, os.MkdirAll(oldInUse, 0o700))
	_, err := runstate.Claim(oldInUse, runstate.Run{Workspace: here})
	require.NoError(t, err)
	lastClaimed(t, oldInUse, 10*24*time.Hour)

	week := clearOptions{verb: "clear", scope: processgroup.InWorkspace(here), olderThan: 7 * 24 * time.Hour}
	preview := week
	preview.dryRun = true
	require.Empty(t, pruneRunState(w, root, preview))
	require.DirExists(t, old, "a dry run removes nothing")

	require.Empty(t, pruneRunState(w, root, week))
	require.NoDirExists(t, old)
	for _, dir := range []string{recent, theirs, oldInUse} {
		require.DirExists(t, dir)
	}
}

func TestClearAgeAcceptsDays(t *testing.T) {
	for value, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "72h": 72 * time.Hour, "1.5d": 36 * time.Hour} {
		got, err := parseStateAge(value)
		require.NoError(t, err, value)
		require.Equal(t, want, got, value)
	}
	for _, value := range []string{"", "0", "-3d", "soon", "0d"} {
		_, err := parseStateAge(value)
		require.Error(t, err, "%q", value)
	}
}
