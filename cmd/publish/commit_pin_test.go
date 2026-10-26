package publish

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A release composed over a commit pin is refused by name at pre-flight,
// before any version is bumped, anything is signed, or a tag reaches origin.
func TestEngine_Release_RefusesAWorkspacePinnedAtACommit(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	dir, origin, _ := releaseRepo(t, "0.1.0")
	workspace := filepath.Join(dir, "workspace.codefly.yaml")
	require.NoError(t, os.WriteFile(workspace, []byte("name: platform\nlayout: modules\nmodules:\n  - name: saas\n    source: codefly-dev/module-saas-starter\n    version: "+sha+"\n"), 0o600))
	gitIn(t, dir, "add", "workspace.codefly.yaml")
	gitIn(t, dir, "commit", "-m", "compose at a commit")
	gitIn(t, dir, "push", "origin", "main")
	m, err := Detect(dir)
	require.NoError(t, err)

	_, err = (&Engine{Manifest: m, BumpType: "patch", WorkDir: dir}).Release(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "module saas is pinned at commit "+sha+": a commit pin is for validation; release from a tag")
	require.Empty(t, gitIn(t, origin, "tag", "-l"), "nothing reached origin")
	require.Equal(t, "version: 0.1.0\n", string(mustRead(t, filepath.Join(dir, "pkg", "cli", "info.yaml"))), "nothing was bumped")

	// The same workspace pinned at a tag releases.
	require.NoError(t, os.WriteFile(workspace, []byte("name: platform\nlayout: modules\nmodules:\n  - name: saas\n    source: codefly-dev/module-saas-starter\n    version: v0.0.93\n"), 0o600))
	gitIn(t, dir, "add", "workspace.codefly.yaml")
	gitIn(t, dir, "commit", "-m", "compose at a tag")
	gitIn(t, dir, "push", "origin", "main")
	require.NoError(t, (&Engine{Manifest: m, WorkDir: dir}).assertNoCommitPins(context.Background()))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}
