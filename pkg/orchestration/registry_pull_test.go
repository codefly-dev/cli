package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

// remoteRepository is a cell's Artifact Registry remote repository of ghcr.io.
const remoteRepository = "us-east1-docker.pkg.dev/cell/ghcr"

// pullThroughEnv pushes to ghcr.io and pulls through the cell's remote repository.
func pullThroughEnv() *environments.Environment {
	return &environments.Environment{Name: "staging", Registry: &environments.EnvironmentRegistry{
		URL: "ghcr.io/obin-ai", Mirrors: map[string]string{"ghcr.io": remoteRepository},
	}}
}

// recordBuildArgs makes the next `buildx build` write its argv to a file.
func (stub *dockerStub) recordBuildArgs(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(stub.dir, "args")
	require.NoError(t, os.WriteFile(filepath.Join(stub.dir, "hook"), []byte(`printf '%s\n' "$*" > '`+path+"'\n"), 0o644))
	return func() string { return strings.TrimSpace(readFile(t, path)) }
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(payload)
}

func TestPushReferenceMovesOnlyImagesUnderThePullRepository(t *testing.T) {
	env := pullThroughEnv()
	require.Equal(t, "ghcr.io/obin-ai/saas/api:1.2.3", pushReference(env, remoteRepository+"/obin-ai/saas/api:1.2.3"))
	require.Equal(t, "docker.io/library/postgres:16", pushReference(env, "docker.io/library/postgres:16"), "a foreign image is untouched")
	require.Equal(t, remoteRepository+"/obin-ai-other/api:1", pushReference(env, remoteRepository+"/obin-ai-other/api:1"), "a prefix is a whole path component")
	unmirrored := &environments.Environment{Registry: &environments.EnvironmentRegistry{URL: "ghcr.io/obin-ai"}}
	require.Equal(t, "ghcr.io/obin-ai/saas/api:1", pushReference(unmirrored, "ghcr.io/obin-ai/saas/api:1"))
	require.Equal(t, "x/api:1", pushReference(nil, "x/api:1"))
}

func TestRecipeAsBuiltNamesThePushedReferenceAndLeavesThePlanAlone(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{Name: "api", Image: remoteRepository + "/obin-ai/saas/api:1.2.3", Platforms: []string{"linux/amd64"}}
	builder := &Builder{world: &World{Env: pullThroughEnv()}}
	built, err := builder.recipeAsBuilt(recipe, true)
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/obin-ai/saas/api:1.2.3", built.GetImage())
	require.Equal(t, remoteRepository+"/obin-ai/saas/api:1.2.3", recipe.GetImage(), "the plan keeps the name the cell pulls")
	local, err := builder.recipeAsBuilt(recipe, false)
	require.NoError(t, err)
	require.Same(t, recipe, local, "a build that does not push is not renamed")
}

// A released version is probed, tagged and pushed at ghcr.io; the digest the
// manifests pin is the pushed one.
func TestAReleasedVersionIsProbedAndPushedAtTheRegistryItPullsThrough(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	harness.env = pullThroughEnv()
	harness.peer.image = remoteRepository + "/obin-ai/saas/api:1.2.3"
	var probed []string
	previous := resolveImageManifestDigest
	t.Cleanup(func() { resolveImageManifestDigest = previous })
	resolveImageManifestDigest = func(ctx context.Context, reference, builder string) (string, error) {
		if strings.HasSuffix(reference, ":1.2.3") {
			probed = append(probed, reference)
			return "", fmt.Errorf("%w: not published", errImageNotFound)
		}
		return previous(ctx, reference, builder)
	}
	args := harness.docker.recordBuildArgs(t)
	require.NotEmpty(t, harness.build(t))
	require.Equal(t, []string{"ghcr.io/obin-ai/saas/api:1.2.3"}, probed)
	require.Contains(t, args(), "-t ghcr.io/obin-ai/saas/api:1.2.3")
	require.NotContains(t, args(), remoteRepository)
}

// Only the registry saying "no such tag" lets a released version be built: any
// other failed probe could overwrite a released tag.
func TestAReleasedVersionProbeThatFailsOtherwiseStopsTheRender(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	harness.peer.image = "example.test/app:1.2.3"
	previous := resolveImageManifestDigest
	t.Cleanup(func() { resolveImageManifestDigest = previous })
	resolveImageManifestDigest = func(context.Context, string, string) (string, error) {
		return "", errors.New("denied: unauthorized")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	require.ErrorContains(t, harness.tryBuild(ctx, t), "cannot tell whether example.test/app:1.2.3 is already published")
	require.Equal(t, 0, harness.docker.calls(t, "build"))
}

func TestImageProbeClassifiesOnlyARegistryNotFoundAsAbsent(t *testing.T) {
	probe := func(stderr string) error {
		_, err := exec.Command("sh", "-c", "printf '%s' \"$0\" >&2; exit 1", stderr).Output()
		return classifyImageProbeError(err)
	}
	require.True(t, isImageNotFound(probe("ERROR: ghcr.io/obin-ai/api:1.2.3: not found")))
	require.True(t, isImageNotFound(probe("MANIFEST_UNKNOWN: manifest unknown")))
	unauthorized := probe("ERROR: failed to authorize: 401 Unauthorized")
	require.False(t, isImageNotFound(unauthorized))
	require.ErrorContains(t, unauthorized, "401 Unauthorized")
	require.False(t, isImageNotFound(classifyImageProbeError(context.DeadlineExceeded)))
}

// The source label links a ghcr.io package to the workspace's repository; it
// is added after the identity, so a different remote reuses the same image.
func TestSourceLabelLinksAGHCRImageAndIsNotPartOfTheIdentity(t *testing.T) {
	previous := workspaceSourceRepository
	t.Cleanup(func() { workspaceSourceRepository = previous })
	source := "https://github.com/obin-ai/platform"
	workspaceSourceRepository = func(context.Context, string) string { return source }

	harness := newImageCacheHarness(t, true, true)
	harness.env = &environments.Environment{Registry: &environments.EnvironmentRegistry{URL: "ghcr.io/obin-ai"}}
	args := harness.docker.recordBuildArgs(t)
	first := harness.build(t)
	require.Contains(t, args(), "--label "+imageSourceLabel+"="+source)
	key := harness.key(t)

	source = "https://github.com/obin-ai/elsewhere"
	require.Equal(t, first, harness.build(t))
	require.Equal(t, 1, harness.docker.calls(t, "build"), "the source label is not an image input")
	require.Equal(t, key, harness.key(t))

	base := []string{"buildx", "build", "--push", "."}
	require.Equal(t, base, withSourceLabel(base, &environments.Environment{Registry: &environments.EnvironmentRegistry{URL: "registry.example/team"}}, source, false))
	require.Equal(t, []string{"buildx", "build", "--label", imageSourceLabel + "=" + source, "--annotation", "index:" + imageSourceLabel + "=" + source, "--push", "."},
		withSourceLabel(base, harness.env, source, true))
	require.Equal(t, base, withSourceLabel(base, harness.env, "", true))
	for remote, want := range map[string]string{
		"git@github.com:obin-ai/platform.git":         "https://github.com/obin-ai/platform",
		"https://github.com/obin-ai/platform.git":     "https://github.com/obin-ai/platform",
		"ssh://git@github.com/obin-ai/platform":       "https://github.com/obin-ai/platform",
		"https://gitlab.example/obin-ai/platform.git": "",
	} {
		match := githubRemote.FindStringSubmatch(remote)
		got := ""
		if match != nil {
			got = "https://github.com/" + match[1] + "/" + match[2]
		}
		require.Equal(t, want, got, remote)
	}
}

// An image an agent built in-process is tagged and pushed at ghcr.io.
func TestLegacyPushTagsAndPushesThePushedReference(t *testing.T) {
	previous := runDocker
	t.Cleanup(func() { runDocker = previous })
	var calls []string
	runDocker = func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	pulled := remoteRepository + "/obin-ai/saas/api:1.2.3"
	require.NoError(t, pushLegacyImage(t.Context(), pullThroughEnv(), pulled))
	require.Equal(t, []string{"tag " + pulled + " ghcr.io/obin-ai/saas/api:1.2.3", "push ghcr.io/obin-ai/saas/api:1.2.3"}, calls)

	calls = nil
	require.NoError(t, pushLegacyImage(t.Context(), nil, "registry.example/team/api:1"))
	require.Equal(t, []string{"push registry.example/team/api:1"}, calls, "an unmirrored registry pushes as before")
}
