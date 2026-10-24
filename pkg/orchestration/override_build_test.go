package orchestration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// stubRegistry makes the registry answer for exactly the references it holds,
// each with its digest and build labels; any other reference is not published.
func stubRegistry(t *testing.T, held map[string]publishedImage) {
	t.Helper()
	previousDigest, previousLabels := resolveImageManifestDigest, resolveImageBuildLabels
	t.Cleanup(func() { resolveImageManifestDigest, resolveImageBuildLabels = previousDigest, previousLabels })
	resolveImageManifestDigest = func(ctx context.Context, reference, builder string) (string, error) {
		if image, ok := held[reference]; ok {
			return image.digest, nil
		}
		if strings.Contains(reference, "@") {
			return previousDigest(ctx, reference, builder)
		}
		return "", fmt.Errorf("%w: not published", errImageNotFound)
	}
	resolveImageBuildLabels = func(_ context.Context, reference, _ string) (map[string]string, error) {
		return held[reference].labels, nil
	}
}

type publishedImage struct {
	digest string
	labels map[string]string
}

var qualifiedTag = regexp.MustCompile(`-t example\.test/app:1\.2\.3-build\.[0-9a-f]{16} `)

// The obin-blue defect: a release is published, the workspace overrides the
// agent its services build with, and the render refused the release's own
// version as "not moved with its code". The overridden build is another build,
// so it is pushed under a tag of its own — never the release's — is not
// refused, and is deployed by the digest it was pushed at even where the flow
// asked for no digest.
func TestAnOverriddenAgentBuildOfAPublishedVersionIsTaggedApart(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	harness.peer.image = "example.test/app:1.2.3"
	harness.agentOverride = "0.2.0"
	harness.noCapture = true
	release := publishedImage{digest: "sha256:" + strings.Repeat("d", 64), labels: map[string]string{
		buildInputsLabel: "sha256:" + strings.Repeat("e", 64), buildAgentLabel: "codefly.dev/go:0.1.0",
	}}
	stubRegistry(t, map[string]publishedImage{"example.test/app:1.2.3": release})

	digest := harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"))
	require.NotEqual(t, release.digest, digest, "the release's build is not the overridden one")
	args := harness.docker.buildArgs(t)
	require.Len(t, args, 1)
	require.Regexp(t, qualifiedTag, args[0], "pushed under a build-qualified tag")
	require.NotContains(t, args[0], "-t example.test/app:1.2.3 ", "never under the release's tag")
	require.Contains(t, args[0], "--label "+buildAgentLabel+"=codefly.dev/go:0.2.0")

	// Another checkout composing the same override adopts that build from the
	// registry by its qualified tag instead of pushing a second one over it.
	key := harness.key(t)
	qualified := overrideBuildImage("example.test/app:1.2.3", key)
	require.Contains(t, args[0], "-t "+qualified+" ")
	require.NoError(t, os.RemoveAll(filepath.Join(harness.workspaceDir, filepath.FromSlash(imageCacheDir))))
	stubRegistry(t, map[string]publishedImage{
		"example.test/app:1.2.3": release,
		qualified:                {digest: digest, labels: map[string]string{buildInputsLabel: key}},
	})
	require.Equal(t, digest, harness.build(t))
	require.Equal(t, 1, harness.docker.calls(t, "build"), "the qualified build is adopted, not rebuilt")
}

// An override entry naming the version the module already pins moves nothing:
// the build is the release's, under the release's tag.
func TestAnOverrideToTheModulesOwnPinIsTheReleaseBuild(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	harness.peer.image = "example.test/app:1.2.3"
	harness.agentOverride = "0.1.0"
	stubRegistry(t, nil)
	harness.build(t)
	args := harness.docker.buildArgs(t)
	require.Len(t, args, 1)
	require.Contains(t, args[0], "-t example.test/app:1.2.3 ")
}

// classLabels reads the input labels a build recorded off its argv.
func classLabels(t *testing.T, args string) map[string]string {
	t.Helper()
	labels := map[string]string{}
	for _, match := range regexp.MustCompile(`--label (org\.codefly\.build\.[a-z.]+)=(\S+)`).FindAllStringSubmatch(args, -1) {
		labels[match[1]] = match[2]
	}
	for _, key := range []string{buildInputsLabel, buildCodeLabel, buildRecipeLabel, buildBasesLabel, buildAgentLabel} {
		require.NotEmpty(t, labels[key], "every build records %s", key)
	}
	return labels
}

// The 2026-10-24 defect: a release published from another checkout, whose
// recipe or bases differ from this one's, refused every later render of every
// module on the cell. The refusal exists for a version not moved with its
// code, so only a code difference refuses; anything else adopts the release's
// build. An agent other than the release's pin still refuses (#1146).
func TestAPublishedReleaseIsJudgedByItsCode(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	harness.peer.image = "example.test/app:1.2.3"
	stubRegistry(t, nil)
	harness.build(t)
	args := harness.docker.buildArgs(t)
	require.Len(t, args, 1)
	ours := classLabels(t, args[0])
	other := "sha256:" + strings.Repeat("e", 64)
	published := "sha256:" + strings.Repeat("d", 64)
	with := func(changes map[string]string) map[string]string {
		labels := map[string]string{}
		for key, value := range ours {
			labels[key] = value
		}
		for key, value := range changes {
			if value == "" {
				delete(labels, key)
				continue
			}
			labels[key] = value
		}
		return labels
	}
	for name, tc := range map[string]struct {
		labels map[string]string
		refuse string
	}{
		"same code, other recipe and bases": {labels: with(map[string]string{buildInputsLabel: other, buildRecipeLabel: other, buildBasesLabel: other})},
		"labelled before the classes": {labels: with(map[string]string{
			buildInputsLabel: other, buildCodeLabel: "", buildRecipeLabel: "", buildBasesLabel: "",
		})},
		"other code":  {labels: with(map[string]string{buildInputsLabel: other, buildCodeLabel: other}), refuse: "is already published from other code"},
		"other agent": {labels: with(map[string]string{buildInputsLabel: other, buildAgentLabel: "codefly.dev/go:0.0.9"}), refuse: "from a build by agent codefly.dev/go:0.0.9"},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.RemoveAll(filepath.Join(harness.workspaceDir, filepath.FromSlash(imageCacheDir))))
			stubRegistry(t, map[string]publishedImage{"example.test/app:1.2.3": {digest: published, labels: tc.labels}})
			if tc.refuse == "" {
				require.Equal(t, published, harness.build(t))
			} else {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				require.ErrorContains(t, harness.tryBuild(ctx, t), tc.refuse)
			}
			require.Equal(t, 1, harness.docker.calls(t, "build"), "a published release is never rebuilt")
		})
	}
}

// The warning says which class differs, and the refusal names the code.
func TestPublishedReleaseVerdictNamesTheClass(t *testing.T) {
	agent := &resources.Agent{Publisher: "codefly.dev", Name: "go", Version: "0.1.0"}
	ours := imageIdentity{Key: "sha256:k", Classes: &buildClasses{Code: "sha256:c", Recipe: "sha256:r", Bases: "sha256:b"}}
	labels := func(code, recipe, bases string) map[string]string {
		return map[string]string{buildInputsLabel: "sha256:other", buildCodeLabel: code, buildRecipeLabel: recipe, buildBasesLabel: bases}
	}
	warning, err := publishedReleaseVerdict("app:1.2.3", ours, agent, labels("sha256:c", "sha256:x", "sha256:b"))
	require.NoError(t, err)
	require.Contains(t, warning, "the build recipe its agent emitted")
	require.NotContains(t, warning, "base images")
	warning, err = publishedReleaseVerdict("app:1.2.3", ours, agent, labels("sha256:c", "sha256:r", "sha256:x"))
	require.NoError(t, err)
	require.Contains(t, warning, "the resolved base images")
	require.NotContains(t, warning, "recipe")
	warning, err = publishedReleaseVerdict("app:1.2.3", ours, agent, map[string]string{buildInputsLabel: "sha256:other"})
	require.NoError(t, err)
	require.Contains(t, warning, "predates per-class build labels")
	_, err = publishedReleaseVerdict("app:1.2.3", ours, agent, labels("sha256:x", "sha256:r", "sha256:b"))
	require.ErrorContains(t, err, "app:1.2.3 is already published from other code")
	require.ErrorContains(t, err, "release a new version")
}

func TestOverrideBuildImageIsAValidDockerTag(t *testing.T) {
	image := overrideBuildImage("registry.example.test/compute/excel:0.3.2", "sha256:"+strings.Repeat("ab", 32))
	require.Equal(t, "registry.example.test/compute/excel:0.3.2-build.abababababababab", image)
	require.False(t, releasedVersionTag(image), "a qualified tag never reads as the release")
	tag := image[strings.LastIndex(image, ":")+1:]
	require.Regexp(t, `^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`, tag)
	require.Equal(t, []string{"buildx", "build", "--label", buildAgentLabel + "=codefly.dev/go:0.2.0", "."},
		withBuildAgentLabel([]string{"buildx", "build", "."}, &resources.Agent{Publisher: "codefly.dev", Name: "go", Version: "0.2.0"}))
}

// The qualified tag composes with a pull-through registry: the overridden build
// is probed and pushed at ghcr.io under the qualified tag, while the plan — and
// so the deployed manifest the agent renders — keeps the repository the cell
// pulls from, pinned to the pushed digest.
func TestAnOverriddenBuildIsPushedQualifiedAtTheRegistryItPullsThrough(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	harness.env = pullThroughEnv()
	harness.agentOverride = "0.2.0"
	harness.noCapture = true
	pulled := remoteRepository + "/obin-ai/saas/api:1.2.3"
	harness.peer.image = pulled
	stubRegistry(t, map[string]publishedImage{"ghcr.io/obin-ai/saas/api:1.2.3": {
		digest: "sha256:" + strings.Repeat("d", 64), labels: map[string]string{buildInputsLabel: "sha256:" + strings.Repeat("e", 64)},
	}})

	digest := harness.build(t)
	args := harness.docker.buildArgs(t)
	require.Len(t, args, 1)
	pushed := overrideBuildImage("ghcr.io/obin-ai/saas/api:1.2.3", harness.key(t))
	require.Contains(t, args[0], "-t "+pushed+" ", "pushed at ghcr.io under the qualified tag")
	require.NotContains(t, args[0], remoteRepository)
	require.NotContains(t, args[0], "-t ghcr.io/obin-ai/saas/api:1.2.3 ")
	// The agent renders the deployed image from its own plan name (the pull
	// repository) and this digest; the cell keeps that repository and digest.
	require.Equal(t, "sha256:"+fmt.Sprintf("%064d", 1), digest, "the digest the deployment pins is the pushed one")
}
