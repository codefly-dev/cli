package solutionrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A staged copy whose removal is interrupted keeps part of its node_modules.
// The next build must install again rather than trust a record that says the
// copy was installed from this lockfile: otherwise it runs `npm run build`
// against a half-deleted node_modules and fails with "command not found".
// Forget clears the record's install before it removes the copy, so whatever
// the removal leaves behind, the next build installs.
func TestForgetClearsTheInstallBeforeRemovingTheCopy(t *testing.T) {
	state := t.TempDir()
	build := LocalFrontend{StateDir: state}
	record := filepath.Join(state, "quill.json")
	require.NoError(t, os.WriteFile(record, []byte(`{"inputs":"in","installed":"lock-digest","bundle":"sha256:b"}`), 0o600))

	// A removal that cannot finish: a directory whose entries cannot be removed.
	stuck := filepath.Join(build.stagingDir("quill"), "source", "node_modules", "kept")
	require.NoError(t, os.MkdirAll(stuck, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stuck, "file"), []byte("x"), 0o600))
	require.NoError(t, os.Chmod(stuck, 0o500))
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o755) })

	_ = build.Forget(context.Background(), "quill")

	data, err := os.ReadFile(record)
	require.NoError(t, err)
	var kept localFrontendRecord
	require.NoError(t, json.Unmarshal(data, &kept))
	require.Empty(t, kept.Installed, "a copy that may be partly removed is never trusted as installed")
	require.Equal(t, "sha256:b", kept.Bundle, "the retained bundle is still reused")
	require.Equal(t, "in", kept.Inputs)
}
