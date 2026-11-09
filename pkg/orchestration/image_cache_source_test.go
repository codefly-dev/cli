package orchestration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

// Every file Docker is sent is an input, whatever the repository's own rules
// say: a file the repository ignores but the recipe's policy lets through —
// a generated configuration, a local run's binary — reaches the image, so it
// moves the identity, and reuse never keeps an image built from other inputs.
// Scratch that should not reach an image is the recipe's policy's to drop.
func TestAFileDockerIsSentIsAnInputWhateverTheRepositoryIgnores(t *testing.T) {
	fixture := newIdentityFixture(t, "FROM scratch\nCOPY code /app\n", "Dockerfile")
	gitRepository(t, fixture.scope.WorkspaceDir, ".cache/", "__pycache__/", "*.log", "/svc/code/generated.json")
	for name, write := range map[string]func(){
		"a native build's binary":   func() { fixture.write(t, ".cache/native/main", "ELF") },
		"a bytecode cache":          func() { fixture.write(t, "code/__pycache__/main.cpython-313.pyc", "pyc") },
		"a generated configuration": func() { fixture.write(t, "code/generated.json", "{}") },
		"a log file":                func() { fixture.write(t, "run.log", "started\n") },
	} {
		t.Run(name, func(t *testing.T) {
			before := fixture.key(t, []string{"Dockerfile"})
			write()
			require.NotEqual(t, before, fixture.key(t, []string{"Dockerfile"}), "a file Docker is sent is an input")
		})
	}
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

// A developer's checkout whose repository ignores a generated input the
// Dockerfile copies: changing that input rebuilds the image rather than
// keeping the one built from the old content.
func TestAnIgnoredInputDockerIsSentInvalidatesReuse(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	t.Setenv(resources.CodeflyHomeEnv, harness.workspaceDir)
	t.Setenv(modulecache.Env, "")
	gitRepository(t, harness.workspaceDir, "generated.json")
	stubRegistry(t, nil)
	harness.write(t, filepath.Join(harness.serviceDir, "code", "generated.json"), `{"version": 1}`)
	harness.build(t)
	harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"), "unchanged inputs reuse the image")

	harness.write(t, filepath.Join(harness.serviceDir, "code", "generated.json"), `{"version": 2}`)
	harness.build(t)
	require.Equal(t, 2, harness.docker.calls(t, "build"), "a changed input Docker is sent rebuilds the image")
}
