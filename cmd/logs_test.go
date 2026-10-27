package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

func TestLogSelectionKeepsTheLatestInvocationDespiteOlderWritesAndRotation(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	root := cli.LogsDir()
	require.NoError(t, os.MkdirAll(root, 0o700))
	legacy := filepath.Join(root, "9999-12-31.log")
	require.NoError(t, os.WriteFile(legacy, []byte("legacy"), 0o600))
	old, err := cli.NewFileLogger(root)
	require.NoError(t, err)
	defer old.Close()
	fresh, err := cli.NewFileLogger(root)
	require.NoError(t, err)
	defer fresh.Close()
	fresh.Process(&wool.Log{Level: wool.INFO, Message: "fresh startup"})
	old.Process(&wool.Log{Level: wool.INFO, Message: "old shutdown"})
	// A later old-run segment must never win over the newer invocation.
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(old.Path()), "9999-12-31-999999.log"), []byte("old rotated"), 0o600))
	got, err := resolveLogFile()
	require.NoError(t, err)
	require.Equal(t, fresh.Path(), got)
	latestSegment := filepath.Join(filepath.Dir(fresh.Path()), "9999-12-31-999999.log")
	require.NoError(t, os.WriteFile(latestSegment, []byte("fresh rotated"), 0o600))
	got, err = resolveLogFile()
	require.NoError(t, err)
	require.Equal(t, latestSegment, got)
	// Explicit paths and previously opened descriptors remain usable.
	require.Equal(t, []string{"legacy"}, tailLines(legacy, 10))
	file, err := os.Open(fresh.Path())
	require.NoError(t, err)
	defer file.Close()
	before, err := file.Stat()
	require.NoError(t, err)
	fresh.Process(&wool.Log{Level: wool.INFO, Message: "fresh still writing"})
	after, err := file.Stat()
	require.NoError(t, err)
	require.Greater(t, after.Size(), before.Size())
}

func TestLogSelectionReadsLegacyFilesWithoutCreatingAnInvocation(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	_, err := resolveLogFile()
	require.ErrorContains(t, err, "no logs found")
	require.NoError(t, os.MkdirAll(cli.LogsDir(), 0o700))
	legacy := filepath.Join(cli.LogsDir(), "2026-01-01.log")
	require.NoError(t, os.WriteFile(legacy, []byte("legacy"), 0o600))
	got, err := resolveLogFile()
	require.NoError(t, err)
	require.Equal(t, legacy, got)
	entries, err := os.ReadDir(cli.LogsDir())
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
