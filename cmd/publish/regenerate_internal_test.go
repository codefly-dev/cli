package publish

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func regenGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := command.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

// regenerationRepo is a repository with a release manifest and a generated
// tree: one file the regeneration rewrites, one it deletes, and none of the
// file it adds.
func regenerationRepo(t *testing.T) (dir, tree string) {
	t.Helper()
	dir = t.TempDir()
	origin := t.TempDir()
	regenGit(t, origin, "init", "--bare", "-b", "main")
	regenGit(t, dir, "init", "-b", "main")
	for _, kv := range [][2]string{{"commit.gpgsign", "false"}, {"tag.gpgsign", "false"}, {"user.email", "test@example.com"}, {"user.name", "Test"}, {"gc.auto", "0"}} {
		regenGit(t, dir, "config", kv[0], kv[1])
	}
	regenGit(t, dir, "remote", "add", "origin", origin)
	tree = filepath.Join(dir, "contracts", "runnables")
	require.NoError(t, os.MkdirAll(tree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "info.codefly.yaml"), []byte("version: 0.1.0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "index.json"), []byte("0.1.0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "gone.json"), []byte("old\n"), 0o600))
	regenGit(t, dir, "add", ".")
	regenGit(t, dir, "commit", "-m", "seed")
	regenGit(t, dir, "push", "-u", "origin", "main")
	return dir, tree
}

func regeneratingEngine(t *testing.T, dir, tree string) *Engine {
	t.Helper()
	manifest, err := Detect(dir)
	require.NoError(t, err)
	return &Engine{
		Manifest: manifest, BumpType: "minor", WorkDir: dir,
		generated: []string{tree},
		regenerate: func(context.Context) error {
			if err := os.WriteFile(filepath.Join(tree, "index.json"), []byte("0.2.0\n"), 0o600); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(tree, "gone.json")); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(tree, "added.json"), []byte("new\n"), 0o600)
		},
	}
}

// The regenerated tree is committed whole with the manifests — rewritten,
// deleted and added files alike — in the one release commit.
func TestReleaseCommitsTheRegeneratedTree(t *testing.T) {
	dir, tree := regenerationRepo(t)
	tag, err := regeneratingEngine(t, dir, tree).Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.2.0", tag)
	changed := strings.Fields(regenGit(t, dir, "show", "--name-status", "--format=", "HEAD"))
	require.Equal(t, []string{
		"A", "contracts/runnables/added.json",
		"D", "contracts/runnables/gone.json",
		"M", "contracts/runnables/index.json",
		"M", "info.codefly.yaml",
	}, changed)
	require.Empty(t, strings.TrimSpace(regenGit(t, dir, "status", "--porcelain")))
}

// A release that aborts after regenerating leaves the tree exactly as HEAD
// has it: rewritten files restored, the deleted one back, the added one gone.
func TestAbortedReleaseRestoresTheRegeneratedTree(t *testing.T) {
	dir, tree := regenerationRepo(t)
	engine := regeneratingEngine(t, dir, tree)
	ciFailed := errors.New("release-grade agent CI failed")
	engine.BeforeCommit = func(context.Context, string) error { return ciFailed }

	_, err := engine.Release(context.Background())
	require.ErrorIs(t, err, ciFailed)
	require.Empty(t, strings.TrimSpace(regenGit(t, dir, "status", "--porcelain", "--untracked-files=all")))
	index, err := os.ReadFile(filepath.Join(tree, "index.json"))
	require.NoError(t, err)
	require.Equal(t, "0.1.0\n", string(index))
}

// A regeneration that fails restores everything the release wrote.
func TestFailedRegenerationRestoresTheManifests(t *testing.T) {
	dir, tree := regenerationRepo(t)
	engine := regeneratingEngine(t, dir, tree)
	engine.regenerate = func(context.Context) error {
		_ = os.WriteFile(filepath.Join(tree, "added.json"), []byte("half\n"), 0o600)
		return errors.New("the committed contracts are stale")
	}
	_, err := engine.Release(context.Background())
	require.ErrorContains(t, err, "regenerate what carries the version at v0.2.0")
	require.Empty(t, strings.TrimSpace(regenGit(t, dir, "status", "--porcelain", "--untracked-files=all")))
}
