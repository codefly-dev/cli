//go:build integration

package orchestration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// lockedGitSource is a repository URL nothing resolves: only the host's git
// configuration reaches it, standing in for a private repository whose
// credential the host holds and the image build does not.
const lockedGitSource = "https://git.invalid/team/lib.git"

// gitSourceBuildImage is the image the build test fetches in: git, pinned.
const gitSourceBuildImage = "alpine/git:v2.52.0@sha256:4a0e72d49596a1f5d3701aeedafdadc5c0da4062be4657c7bdc4017387f591cc"

// reachLockedSourceThroughGit points the host's git at the repository on disk,
// as a developer's or CI job's insteadOf points it at a private repository.
// extra is further host configuration.
func reachLockedSourceThroughGit(t *testing.T, repository, extra string) {
	t.Helper()
	configuration := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(configuration, []byte(fmt.Sprintf(
		"[url \"%s\"]\n\tinsteadOf = %s\n%s", repository, lockedGitSource, extra)), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", configuration)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// TestALockedGitSourceIsMirroredWithTheHostsGitConfiguration fetches a commit
// only the host's git configuration reaches, by id and, from a host that
// refuses an unadvertised commit (protocol v0), through the fallback, and
// checks the mirror holds it where a build looks for it and is handed to the
// build as the declared context.
func TestALockedGitSourceIsMirroredWithTheHostsGitConfiguration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("the prefetch runs the host git")
	}
	for name, extra := range map[string]string{
		"by commit id":                  "",
		"through the branches and tags": "[protocol]\n\tversion = 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			repository, locked := gitSourceRepository(t, "sha1")
			reachLockedSourceThroughGit(t, repository, extra)

			prefetch := newGitSourcePrefetch()
			t.Cleanup(func() { require.NoError(t, prefetch.Close()) })
			mirrors, err := prefetch.mirrorsFor(ctx, &builderv0.DockerBuildRecipe{Name: "app", GitSourceDownloads: []*builderv0.GitSourceDownload{
				{Repository: lockedGitSource, Commit: locked, MirrorContext: "gitsources"},
			}})
			require.NoError(t, err)

			mirror := filepath.Join(mirrors["gitsources"], coreservices.GitSourceMirrorPath(lockedGitSource))
			out, err := exec.Command("git", "--git-dir="+mirror, "rev-parse", "--verify", coreservices.GitSourceMirrorRef(locked)).CombinedOutput()
			require.NoError(t, err, string(out))
			require.Equal(t, locked, strings.TrimSpace(string(out)))

			// Only the fallback brings the branches along.
			branch := exec.Command("git", "--git-dir="+mirror, "rev-parse", "--verify", "-q", "refs/heads/main").Run()
			require.Equal(t, extra != "", branch == nil, "the fetch path taken")

			args := privateModuleBuild{}.withProxies(mirrors).buildxArgs(nil)
			require.Equal(t, []string{"--build-context", "gitsources=" + mirrors["gitsources"]}, args)
		})
	}
}

// TestAGitSourceBuildNeedsNoCredentialWithThePrefetchedMirror is the end-to-end
// claim: with no network, the image build fetches the locked commit from the
// mirror context the way a package manager does (the locked URL rewritten to
// the mirror), and without the context the same build fails.
func TestAGitSourceBuildNeedsNoCredentialWithThePrefetchedMirror(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("the prefetch runs the host git")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	repository, locked := gitSourceRepository(t, "sha1")
	reachLockedSourceThroughGit(t, repository, "")

	prefetch := newGitSourcePrefetch()
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })
	recipe := &builderv0.DockerBuildRecipe{
		Name: "app", Dockerfile: "Dockerfile", Context: ".", Image: "codefly-git-source-prefetch-test:latest",
		Platforms:          []string{"linux/" + goArch(t)},
		GitSourceDownloads: []*builderv0.GitSourceDownload{{Repository: lockedGitSource, Commit: locked, MirrorContext: "gitsources"}},
	}
	mirrors, err := prefetch.mirrorsFor(ctx, recipe)
	require.NoError(t, err)

	contextDir := t.TempDir()
	// The consumer's own fetch, as a package manager installing a locked git
	// dependency runs it (uv: `git fetch --force --update-head-ok <url>
	// +<commit>:refs/commit/<commit>`), against the shallow mirror through an
	// insteadOf rewrite, with no network; then the locked file is read.
	dockerfile := fmt.Sprintf(`FROM scratch AS gitsources

FROM %s
RUN --network=none --mount=type=bind,from=gitsources,target=/gitsources \
    git init -q /src && cd /src && \
    git -c safe.directory='*' -c url.file:///gitsources/%s.insteadOf=%s \
        fetch --force --update-head-ok %s '+%s:refs/commit/%s' && \
    git checkout -q %s && test "$(cat lib.txt)" = locked
`, gitSourceBuildImage, coreservices.GitSourceMirrorPath(lockedGitSource), lockedGitSource, lockedGitSource, locked, locked, locked)
	require.NoError(t, os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte(dockerfile), 0o644))

	build := func(private privateModuleBuild) (string, error) {
		args, err := cachedBuildxArgs(recipe, filepath.Join(contextDir, "Dockerfile"), contextDir, false, false, "", "", nil, private)
		require.NoError(t, err)
		require.NotContains(t, args, "--secret", "no credential reaches the build")
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", recipe.GetImage()).Run() })

	out, err := build(privateModuleBuild{}.withProxies(mirrors))
	require.NoError(t, err, "the build must fetch the locked commit from the mirror:\n%s", out)

	out, err = build(privateModuleBuild{})
	require.Error(t, err, "without the mirror the build has no route to the source:\n%s", out)
	require.Contains(t, out, coreservices.GitSourceMirrorPath(lockedGitSource), "it failed at the fetch from the empty mirror context")
}
