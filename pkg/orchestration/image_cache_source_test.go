package orchestration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/modulecache"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// gitRepository makes dir a repository whose rules ignore the given patterns,
// the way a module repository ignores the scratch its agents and local runs
// leave inside a service directory.
func gitRepository(t *testing.T, dir string, ignored ...string) {
	t.Helper()
	init := exec.Command("git", "-C", dir, "init", "-q")
	init.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := init.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(strings.Join(ignored, "\n")+"\n"), 0o644))
}

// The 2026-11-08 defect: a render adopted a pinned release's published image,
// a local run then wrote a compiled binary under the service's `.cache/`, and
// the next render of the same commit refused that image as "already published
// from other code". The Code class is a function of the source tree: what the
// repository ignores is never source, so scratch beside the code moves nothing,
// while a file the repository would carry still moves it.
func TestIgnoredScratchInsideTheContextIsNotAnInput(t *testing.T) {
	fixture := newIdentityFixture(t, "FROM scratch\nCOPY code /app\n", "Dockerfile")
	gitRepository(t, fixture.scope.WorkspaceDir, ".cache/", "__pycache__/", "*.log", "/svc/code/.venv/")
	before := fixture.key(t, []string{"Dockerfile"})

	for name, write := range map[string]func(){
		"a native build's binary":      func() { fixture.write(t, ".cache/native/main", "ELF") },
		"a bytecode cache in the code": func() { fixture.write(t, "code/__pycache__/main.cpython-313.pyc", "pyc") },
		"a virtual environment":        func() { fixture.write(t, "code/.venv/bin/python", "#!") },
		"a log file":                   func() { fixture.write(t, "run.log", "started\n") },
	} {
		t.Run(name, func(t *testing.T) {
			write()
			require.Equal(t, before, fixture.key(t, []string{"Dockerfile"}), "what the repository ignores is not source")
		})
	}
	fixture.write(t, "code/extra.go", "package main\n")
	require.NotEqual(t, before, fixture.key(t, []string{"Dockerfile"}), "an untracked file the repository would carry is source")
}

// An agent-assembled context lives under the workspace's `.codefly/`, which
// the composition ignores wholesale: the repository's rules say nothing about
// that tree, so every file of it stays an input.
func TestAContextThatIsItselfIgnoredKeepsEveryFile(t *testing.T) {
	workspace := t.TempDir()
	gitRepository(t, workspace, ".codefly/")
	service := filepath.Join(workspace, ".codefly", "build", "api", "tree")
	output := filepath.Join(service, "builder")
	require.NoError(t, os.MkdirAll(filepath.Join(service, "code"), 0o755))
	require.NoError(t, os.MkdirAll(output, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(output, "Dockerfile"), []byte("FROM scratch\nCOPY code /app\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(service, "code", "main.go"), []byte("package main\n"), 0o644))
	fixture := &identityFixture{service: service, output: output, scope: imageBuildScope{
		OutputDir: output, ContextDir: resolvedDir(t.Context(), service), WorkspaceDir: workspace,
	}}
	require.Empty(t, sourceIgnoredPaths(t.Context(), fixture.scope.ContextDir))
	before := fixture.key(t, []string{"Dockerfile"})
	fixture.write(t, "code/extra.go", "package main\n")
	require.NotEqual(t, before, fixture.key(t, []string{"Dockerfile"}))
}

// Outside any repository nothing is excluded, as before.
func TestAContextOutsideARepositoryKeepsEveryFile(t *testing.T) {
	fixture := newIdentityFixture(t, "FROM scratch\nCOPY code /app\n", "Dockerfile")
	require.Empty(t, sourceIgnoredPaths(t.Context(), fixture.scope.ContextDir))
	before := fixture.key(t, []string{"Dockerfile"})
	fixture.write(t, ".cache/native/main", "ELF")
	require.NotEqual(t, before, fixture.key(t, []string{"Dockerfile"}))
}

// The same pinned commit, rendered after the local reuse records were lost and
// after a local run left its scratch in the service directory: the published
// image records this build's inputs and is adopted, and nothing is built. The
// release verdict still refuses when a source file did change.
func TestAPinnedReleaseIsAdoptedAfterTheCacheIsLostDespiteLocalScratch(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	t.Setenv(resources.CodeflyHomeEnv, harness.workspaceDir)
	t.Setenv(modulecache.Env, "")
	gitRepository(t, harness.workspaceDir, ".cache/")
	harness.peer.image = "example.test/app:1.2.3"
	stubRegistry(t, nil)
	harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"))
	args := harness.docker.buildArgs(t)
	require.Len(t, args, 1)
	labels := classLabels(t, args[0])
	require.Equal(t, harness.key(t), labels[buildInputsLabel])

	require.NoError(t, os.RemoveAll(filepath.Join(harness.workspaceDir, filepath.FromSlash(imageCacheDir))), "the local reuse record is gone")
	harness.write(t, filepath.Join(harness.serviceDir, ".cache", "native", "main"), "ELF")
	published := "sha256:" + strings.Repeat("d", 64)
	stubRegistry(t, map[string]publishedImage{"example.test/app:1.2.3": {digest: published, labels: labels}})
	require.Equal(t, published, harness.build(t), "the published build of this commit is adopted")
	require.Equal(t, 1, harness.docker.calls(t, "build"), "nothing is rebuilt")

	harness.write(t, filepath.Join(harness.serviceDir, "code", "main.go"), "package main\n\nfunc main() { println() }\n")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	require.ErrorContains(t, harness.tryBuild(ctx, t), "is already published from other code")
}
