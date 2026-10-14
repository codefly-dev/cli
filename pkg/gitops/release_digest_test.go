package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func writeReleaseFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

// A checkout's release is what its repository tracks or does not ignore:
// dependency caches, built bundles and local secrets the repository ignores,
// the CLI's own state root, render output and version control metadata are
// not the release, and writing to them does not change it; a new source file
// does. The digest is the one the same files have outside a checkout.
func TestReleaseDigestIsTheRepositorysOwnSelectionOfTheModule(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	ctx := context.Background()
	repo := t.TempDir()
	gitInRepo(t, repo, "init", "-q")
	module := filepath.Join(repo, "solutions", "wiki")
	source := map[string]string{
		"module.codefly.yaml":                   "kind: module\nname: wiki\n",
		"module.package.codefly.yaml":           "id: example/wiki\nversion: 1.0.0\n",
		"services/backend/service.codefly.yaml": "kind: service\nname: backend\n",
		".gitignore":                            "node_modules/\nservices/fe-remote/dist/\nconfigurations/local/*.secret.env\n",
	}
	writeReleaseFiles(t, module, source)
	gitInRepo(t, repo, "add", "-A")
	gitInRepo(t, repo, "commit", "-q", "-m", "wiki")
	// What the repository ignores, what the CLI writes, and render output.
	writeReleaseFiles(t, module, map[string]string{
		"services/fe-remote/node_modules/left-pad/index.js": "module.exports = 1",
		"services/fe-remote/dist/mf-manifest.json":          `{"name":"wiki"}`,
		"configurations/local/db.secret.env":                "PASSWORD=hunter2",
		".codefly/native-cluster/local/delivered/x.json":    "{}",
		".codefly/selection-artifacts/0123":                 "archive bytes",
		"deployments/environments/local/x.yaml":             "kind: ConfigMap",
	})
	digest, err := releaseDigest(ctx, module)
	require.NoError(t, err)

	// The same selection outside a checkout: only the package's files.
	plain := t.TempDir()
	writeReleaseFiles(t, plain, source)
	expected, err := releaseDigest(ctx, plain)
	require.NoError(t, err)
	require.Equal(t, expected, digest, "a checkout's release is the identity its package files have on their own")

	// Writing into what the repository ignores or the CLI owns changes nothing.
	writeReleaseFiles(t, module, map[string]string{
		"services/fe-remote/node_modules/left-pad/index.js": "module.exports = 2",
		".codefly/native-cluster/local/delivered/y.json":    "{}",
		"deployments/environments/local/y.yaml":             "kind: Job",
	})
	again, err := releaseDigest(ctx, module)
	require.NoError(t, err)
	require.Equal(t, digest, again, "run state, caches and render output are not the release")

	// A new, not yet committed source file is the release as the developer
	// sees it; a tracked file deleted from the tree is no longer part of it.
	writeReleaseFiles(t, module, map[string]string{"services/backend/main.go": "package main\n"})
	changed, err := releaseDigest(ctx, module)
	require.NoError(t, err)
	require.NotEqual(t, digest, changed, "a new source file is a changed release")
	require.NoError(t, os.Remove(filepath.Join(module, "services", "backend", "main.go")))
	require.NoError(t, os.Remove(filepath.Join(module, "module.package.codefly.yaml")))
	deleted, err := releaseDigest(ctx, module)
	require.NoError(t, err)
	require.NotEqual(t, digest, deleted)
	withoutManifest := t.TempDir()
	delete(source, "module.package.codefly.yaml")
	writeReleaseFiles(t, withoutManifest, source)
	expectedDeleted, err := releaseDigest(ctx, withoutManifest)
	require.NoError(t, err)
	require.Equal(t, expectedDeleted, deleted, "a file deleted from the tree is not digested from the index")
}

// Outside a checkout the whole materialized tree is the package, less the
// CLI's own state and render output.
func TestReleaseDigestOutsideACheckoutIsTheWholeTreeLessTheCLIsOwnOutput(t *testing.T) {
	ctx := context.Background()
	module := t.TempDir()
	writeReleaseFiles(t, module, map[string]string{"module.codefly.yaml": "kind: module\nname: wiki\n", "services/api/main.go": "package main\n"})
	digest, err := releaseDigest(ctx, module)
	require.NoError(t, err)
	writeReleaseFiles(t, module, map[string]string{".codefly/selection-artifacts/abcd": "bytes", "deployments/x.yaml": "x", ".codefly-render-1/y.yaml": "y"})
	again, err := releaseDigest(ctx, module)
	require.NoError(t, err)
	require.Equal(t, digest, again)
	writeReleaseFiles(t, module, map[string]string{"services/api/other.go": "package main\n"})
	changed, err := releaseDigest(ctx, module)
	require.NoError(t, err)
	require.NotEqual(t, digest, changed)
	require.True(t, packageFileExcluded(".codefly/native-cluster/local/x"))
	require.True(t, packageFileExcluded("deployments/x"))
	require.True(t, packageFileExcluded("services/.codefly-render-1/x"))
	require.False(t, packageFileExcluded("services/api/main.go"))
	require.False(t, packageFileExcluded("configurations/local/app.env"), "what a repository does not ignore is the repository's call, not this rule's")
}
