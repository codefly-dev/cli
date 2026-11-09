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
	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := command.CombinedOutput()
	require.NoError(t, err, string(out))
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

// commitPinnedService lays out a service the way a module commits one that
// tracks its agent's cache files under an ignored `.cache/` and ignores the
// logging configuration its agent writes on every build, and commits it.
func commitPinnedService(t *testing.T, repository, service string) {
	t.Helper()
	runGit(t, repository, "init", "-q")
	writeFile(t, filepath.Join(service, ".gitignore"), "/.cache/\n", 0o644)
	writeFile(t, filepath.Join(service, "code", ".gitignore"), "uvicorn-log-config.json\n.venv/\n", 0o644)
	writeFile(t, filepath.Join(service, ".cache", "local", "api.hash"), "committed\n", 0o644)
	writeFile(t, filepath.Join(service, "code", "main.py"), "print('serving')\n", 0o644)
	writeFile(t, filepath.Join(service, "code", "run.sh"), "#!/bin/sh\n", 0o755)
	require.NoError(t, os.Symlink("main.py", filepath.Join(service, "code", "entry.py")))
	runGit(t, repository, "add", "-A")
	runGit(t, repository, "add", "-f", filepath.Join(service, ".cache", "local", "api.hash"))
	runGit(t, repository, "commit", "-q", "-m", "release")
}

// dirtyPinnedService does to a materialized checkout what a local run does: it
// rewrites a tracked cache file, leaves an untracked file, and writes the
// scratch the repository ignores — one entry of which the build needs.
func dirtyPinnedService(t *testing.T, service string) {
	t.Helper()
	writeFile(t, filepath.Join(service, ".cache", "local", "api.hash"), "rewritten by a run\n", 0o644)
	writeFile(t, filepath.Join(service, "code", "extra.py"), "untracked\n", 0o644)
	writeFile(t, filepath.Join(service, ".cache", "native", "main"), "ELF", 0o755)
	writeFile(t, filepath.Join(service, "code", "uvicorn-log-config.json"), "{}\n", 0o644)
	writeFile(t, filepath.Join(service, "code", ".venv", "bin", "python"), "#!", 0o755)
}

type pinnedFixture struct {
	service, output, workspace string
	recipe                     *builderv0.DockerBuildRecipe
}

func newPinnedFixture(t *testing.T, ignorePolicy string) *pinnedFixture {
	t.Helper()
	repository := t.TempDir()
	service := filepath.Join(repository, "modules", "evaluation", "services", "scorer")
	commitPinnedService(t, repository, service)
	// A materialized module's recipe lands under the workspace, outside the
	// checkout (buildRecipeRoot).
	workspace := t.TempDir()
	output := filepath.Join(workspace, ".codefly", "build", "evaluation", "scorer", "builder")
	writeFile(t, filepath.Join(output, "Dockerfile"), "FROM scratch\nCOPY code /app\n", 0o644)
	emitted := []string{"Dockerfile"}
	if ignorePolicy != "" {
		writeFile(t, filepath.Join(output, "dockerignore"), ignorePolicy, 0o644)
		emitted = append(emitted, "dockerignore")
	}
	plan, err := coreservices.SingleImageBuildPlan(output, "example.test/scorer:0.2.4", []string{"linux/amd64"}, emitted)
	require.NoError(t, err)
	return &pinnedFixture{service: service, output: output, workspace: workspace, recipe: plan.GetRecipes()[0]}
}

// digest prepares the context as the builder does and returns its digest and
// the prepared context, closed with the test.
func (fixture *pinnedFixture) digest(t *testing.T, pinned bool) (string, *preparedRecipeContext) {
	t.Helper()
	prepared, err := prepareRecipeContext(t.Context(), fixture.service, fixture.output, fixture.recipe, pinned)
	require.NoError(t, err)
	t.Cleanup(func() { _ = prepared.Close() })
	digest, err := contextDigest(t.Context(), fixture.recipe, prepared.scope(fixture.output, fixture.workspace))
	require.NoError(t, err)
	return digest, prepared
}

// The 2026-11-08 defect, second half: a local run rewrote two files the module
// tracks under its service's `.cache/`, and the next render refused the image
// the pinned commit had published an hour earlier. A pinned release's context is
// its commit: the committed tree digests exactly as a pristine checkout does
// (so it reproduces the published label), and a tracked edit, an untracked
// file and ignored scratch move nothing.
func TestAPinnedReleaseIsItsCommittedTree(t *testing.T) {
	fixture := newPinnedFixture(t, "code/.venv\n")
	pristine, _ := fixture.digest(t, false)
	committed, _ := fixture.digest(t, true)
	require.Equal(t, pristine, committed, "the committed tree digests as a pristine checkout does")

	dirtyPinnedService(t, fixture.service)
	dirty, prepared := fixture.digest(t, true)
	require.Equal(t, pristine, dirty, "nothing a run writes into the cache is the release's code")
	require.NotNil(t, prepared.source)
	require.Equal(t, filepath.Base(fixture.service), filepath.Base(prepared.Root))

	// What the builder is handed is the committed content, plus the ignored
	// build input the agent wrote, minus what the recipe's policy drops.
	read := func(relative string) string {
		payload, err := os.ReadFile(filepath.Join(prepared.Root, filepath.FromSlash(relative)))
		require.NoError(t, err)
		return string(payload)
	}
	require.Equal(t, "committed\n", read(".cache/local/api.hash"))
	require.Equal(t, "print('serving')\n", read("code/main.py"))
	require.NoFileExists(t, filepath.Join(prepared.Root, "code", "extra.py"), "an untracked file is not the release's")
	require.Equal(t, "{}\n", read("code/uvicorn-log-config.json"), "an ignored file the build copies is carried")
	require.Equal(t, "ELF", read(".cache/native/main"))
	require.NoDirExists(t, filepath.Join(prepared.Root, "code", ".venv"), "what the recipe's policy drops is not carried")
	info, err := os.Stat(filepath.Join(prepared.Root, "code", "run.sh"))
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o111, "the committed mode is kept")
	target, err := os.Readlink(filepath.Join(prepared.Root, "code", "entry.py"))
	require.NoError(t, err)
	require.Equal(t, "main.py", target)

	// The checkout itself is left as the run left it, and the copy goes with
	// the build.
	payload, err := os.ReadFile(filepath.Join(fixture.service, ".cache", "local", "api.hash"))
	require.NoError(t, err)
	require.Equal(t, "rewritten by a run\n", string(payload))
	copied := prepared.source.dir
	require.NoError(t, prepared.Close())
	require.NoDirExists(t, copied)
}

// A checkout the developer owns is built from its files: a tracked edit is a
// local change and moves the identity, and so does an ignored file Docker is
// sent, which reaches the image as surely.
func TestADeveloperCheckoutIsItsWorkingTree(t *testing.T) {
	fixture := newPinnedFixture(t, "")
	pristine, prepared := fixture.digest(t, false)
	require.Nil(t, prepared.source)
	require.Equal(t, filepath.Clean(resolvedDir(t.Context(), fixture.service)), prepared.Root)

	writeFile(t, filepath.Join(fixture.service, "code", "uvicorn-log-config.json"), "{}\n", 0o644)
	ignored, _ := fixture.digest(t, false)
	require.NotEqual(t, pristine, ignored, "an ignored file Docker is sent is an input")

	writeFile(t, filepath.Join(fixture.service, ".cache", "local", "api.hash"), "rewritten\n", 0o644)
	edited, _ := fixture.digest(t, false)
	require.NotEqual(t, ignored, edited, "a tracked edit in a developer's checkout is a code change")
}

// A pinned context with no commit to read — the verified-package cache holds no
// repository — is its files.
func TestAPinnedContextOutsideARepositoryIsItsFiles(t *testing.T) {
	fixture := newIdentityFixture(t, "FROM scratch\nCOPY code /app\n", "Dockerfile")
	plan, err := coreservices.SingleImageBuildPlan(fixture.output, "example.test/app:v1", []string{"linux/amd64"}, []string{"Dockerfile"})
	require.NoError(t, err)
	prepared, err := prepareRecipeContext(t.Context(), fixture.service, fixture.output, plan.GetRecipes()[0], true)
	require.NoError(t, err)
	defer prepared.Close()
	require.Nil(t, prepared.source)
	require.Equal(t, fixture.scope.ContextDir, prepared.Root)
}

// End to end through the builder: a service materialized into the module cache
// is built from its commit even after a run dirtied the checkout, the image it
// pushes is labelled with that commit's code, and the next render — the local
// reuse record lost, the checkout dirtied again — adopts the published image
// instead of refusing it. The same edit in a checkout the developer owns
// (a configured cache root is never proof of ownership) is a code change.
func TestAPinnedReleaseIsBuiltAndAdoptedFromItsCommit(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	t.Setenv(resources.CodeflyHomeEnv, harness.workspaceDir)
	t.Setenv(modulecache.Env, "")
	require.NoError(t, os.WriteFile(filepath.Join(harness.workspaceDir, ".gitignore"), []byte(".cache/\n.codefly/\n"), 0o644))
	harness.write(t, filepath.Join(harness.serviceDir, ".cache", "local", "api.hash"), "committed\n")
	runGit(t, harness.workspaceDir, "init", "-q")
	runGit(t, harness.workspaceDir, "add", "-A")
	runGit(t, harness.workspaceDir, "add", "-f", filepath.Join(harness.serviceDir, ".cache", "local", "api.hash"))
	runGit(t, harness.workspaceDir, "commit", "-q", "-m", "release")
	harness.peer.image = "example.test/app:1.2.3"
	stubRegistry(t, nil)

	harness.write(t, filepath.Join(harness.serviceDir, ".cache", "local", "api.hash"), "rewritten by a run\n")
	harness.write(t, filepath.Join(harness.serviceDir, "code", "extra.go"), "package main\n")
	// The stub snapshots the context buildx is handed.
	harness.write(t, filepath.Join(harness.docker.dir, "hook"), `for argument in "$@"; do context="$argument"; done; cp -R "$context" "$STATE/context"`+"\n")
	harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"))
	seen := filepath.Join(harness.docker.dir, "context")
	require.Equal(t, "committed\n", harness.read(t, filepath.Join(seen, ".cache", "local", "api.hash")))
	require.NoFileExists(t, filepath.Join(seen, "code", "extra.go"))
	require.FileExists(t, filepath.Join(seen, "code", "main.go"))
	labels := classLabels(t, harness.docker.buildArgs(t)[0])

	require.NoError(t, os.RemoveAll(filepath.Join(harness.workspaceDir, filepath.FromSlash(imageCacheDir))), "the local reuse record is gone")
	harness.write(t, filepath.Join(harness.serviceDir, ".cache", "local", "api.hash"), "rewritten again\n")
	harness.write(t, filepath.Join(harness.serviceDir, ".cache", "native", "main"), "ELF")
	published := "sha256:" + strings.Repeat("d", 64)
	stubRegistry(t, map[string]publishedImage{"example.test/app:1.2.3": {digest: published, labels: labels}})
	require.Equal(t, published, harness.build(t), "the published build of this commit is adopted")
	require.Equal(t, 1, harness.docker.calls(t, "build"), "nothing is rebuilt")

	t.Setenv(modulecache.Env, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	require.ErrorContains(t, harness.tryBuild(ctx, t), "is already published from other code")
}

// A pinned release's Go modules are fetched from its commit, as Docker is
// handed it and as the identity digests it: a local operation that rewrote
// the module cache checkout's go.mod changes neither what is fetched nor
// what is built.
func TestAPinnedReleasePrefetchesItsCommittedGoModules(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	harness.peer.goModules = true
	t.Setenv(resources.CodeflyHomeEnv, harness.workspaceDir)
	t.Setenv(modulecache.Env, "")
	require.NoError(t, os.WriteFile(filepath.Join(harness.workspaceDir, ".gitignore"), []byte(".codefly/\n"), 0o644))
	committed := harness.read(t, filepath.Join(harness.serviceDir, "code", "go.mod"))
	runGit(t, harness.workspaceDir, "init", "-q")
	runGit(t, harness.workspaceDir, "add", "-A")
	runGit(t, harness.workspaceDir, "commit", "-q", "-m", "release")
	stubRegistry(t, nil)

	harness.write(t, filepath.Join(harness.serviceDir, "code", "go.mod"), committed+"\nrequire example.com/lib v2.0.0+incompatible\n")
	harness.write(t, filepath.Join(harness.docker.dir, "hook"), `for argument in "$@"; do context="$argument"; done; cp -R "$context" "$STATE/context"`+"\n")
	harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"))
	require.NotEmpty(t, harness.goMods, "the declared module graph is fetched")
	for _, read := range harness.goMods {
		require.Equal(t, committed, read, "the prefetch reads the committed go.mod")
	}
	require.Equal(t, committed, harness.read(t, filepath.Join(harness.docker.dir, "context", "code", "go.mod")), "Docker is handed the committed go.mod")
}

// A pinned release's committed copy carries the inputs an agent generated
// beside the commit (a log configuration the Dockerfile copies). They reach
// the image, so a carried input that changed is rebuilt rather than reused;
// they are the agent's outputs, not the release's code, so the Code class the
// published label is judged by stays the commit's.
func TestAPinnedReleaseRebuildsWhenACarriedInputChanges(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	t.Setenv(resources.CodeflyHomeEnv, harness.workspaceDir)
	t.Setenv(modulecache.Env, "")
	require.NoError(t, os.WriteFile(filepath.Join(harness.workspaceDir, ".gitignore"), []byte(".codefly/\ngenerated.json\n"), 0o644))
	runGit(t, harness.workspaceDir, "init", "-q")
	runGit(t, harness.workspaceDir, "add", "-A")
	runGit(t, harness.workspaceDir, "commit", "-q", "-m", "release")
	stubRegistry(t, nil)

	harness.write(t, filepath.Join(harness.serviceDir, "code", "generated.json"), `{"permission":"old"}`)
	harness.build(t)
	harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"), "unchanged inputs reuse the image")
	first := classLabels(t, harness.docker.buildArgs(t)[0])

	harness.write(t, filepath.Join(harness.serviceDir, "code", "generated.json"), `{"permission":"new"}`)
	harness.build(t)
	require.Equal(t, 2, harness.docker.calls(t, "build"), "a changed carried input rebuilds the image")
	second := classLabels(t, harness.docker.buildArgs(t)[1])
	require.NotEqual(t, first[buildInputsLabel], second[buildInputsLabel])
	require.NotEqual(t, first[buildRecipeLabel], second[buildRecipeLabel], "the carried input is the agent's, in the Recipe class")
	require.Equal(t, first[buildCodeLabel], second[buildCodeLabel], "the release's code is its commit")
}
