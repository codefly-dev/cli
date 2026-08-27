package orchestration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func TestBuildxArgsPushIsMultiArchManifestListOnContainerBuilder(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64", "linux/arm64"},
		Target:    "final",
		BuildArgs: map[string]string{"VERSION": "1", "COMMIT": "abc"},
	}
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", true, true, "/tmp/meta.json")
	joined := strings.Join(args, " ")

	require.Equal(t, []string{"buildx", "build"}, args[:2])
	// Multi-platform builds must run on the container-driver builder.
	require.Contains(t, joined, "--builder codefly")
	require.Contains(t, joined, "--platform linux/amd64,linux/arm64")
	require.Contains(t, joined, "--push")
	require.NotContains(t, joined, "--load")
	require.Contains(t, joined, "--metadata-file /tmp/meta.json")
	require.Contains(t, joined, "--target final")
	// Build args are emitted in sorted key order for a stable command.
	require.Contains(t, joined, "--build-arg COMMIT=abc --build-arg VERSION=1")
	require.Equal(t, []string{"-t", "repo/app:v1", "-f", "/svc/builder/Dockerfile", "/svc"}, args[len(args)-5:])
}

func TestBuildxArgsLocalBuildIsSinglePlatformLoadOnDefaultBuilder(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", false, false, "")
	joined := strings.Join(args, " ")

	// A local load cannot materialize a multi-platform manifest list, and it
	// uses the default builder (no dedicated container builder needed).
	require.NotContains(t, joined, "--builder")
	require.Contains(t, joined, "--platform linux/amd64")
	require.NotContains(t, joined, "linux/amd64,linux/arm64")
	require.Contains(t, joined, "--load")
	require.NotContains(t, joined, "--push")
	require.NotContains(t, joined, "--metadata-file")
}

func TestBuildxCreateArgsUsesHostNetworkContainerDriver(t *testing.T) {
	args := buildxCreateArgs()
	joined := strings.Join(args, " ")

	require.Equal(t, []string{"buildx", "create"}, args[:2])
	require.Contains(t, joined, "--name codefly")
	require.Contains(t, joined, "--driver docker-container")
	// Host networking lets BuildKit inherit the host resolver so it can reach a
	// tailnet split-DNS private registry instead of stalling on DNS.
	require.Contains(t, joined, "--driver-opt network=host")
	require.Contains(t, joined, "--bootstrap")
}

func TestPlatformsIncludeDeploymentArch(t *testing.T) {
	require.True(t, platformsIncludeDeploymentArch([]string{"linux/arm64", "linux/amd64"}))
	require.True(t, platformsIncludeDeploymentArch([]string{"linux/amd64/v2"}))
	// An arm64-only recipe would deploy an image that cannot run on amd64 nodes.
	require.False(t, platformsIncludeDeploymentArch([]string{"linux/arm64"}))
	// An empty list builds only the host arch — the arm64-on-Apple-silicon bug.
	require.False(t, platformsIncludeDeploymentArch(nil))
}

func TestRecipeDockerfileResolvesAndContains(t *testing.T) {
	outputDir := filepath.FromSlash("/work/services/store/builder")

	got, err := recipeDockerfile(outputDir, &builderv0.DockerBuildRecipe{Dockerfile: "Dockerfile"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(outputDir, "Dockerfile"), got)

	// A recipe must not point buildx -f at a file outside the recipe tree.
	_, err = recipeDockerfile(outputDir, &builderv0.DockerBuildRecipe{Dockerfile: "../../../../etc/passwd"})
	require.Error(t, err)
}

func TestRecipeContextResolvesAndContains(t *testing.T) {
	serviceDir := "/work/services/store"

	got, err := recipeContext(serviceDir, &builderv0.DockerBuildRecipe{Context: ""})
	require.NoError(t, err)
	require.Equal(t, serviceDir, got)

	got, err = recipeContext(serviceDir, &builderv0.DockerBuildRecipe{Context: "code"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(serviceDir, "code"), got)

	_, err = recipeContext(serviceDir, &builderv0.DockerBuildRecipe{Context: "../other"})
	require.Error(t, err)
}

func TestApplyRecipeIgnoreStagesDiscoverableSibling(t *testing.T) {
	outputDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outputDir, "dockerignore"), []byte("code/node_modules\n"), 0o644))
	dockerfile := filepath.Join(outputDir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile, []byte("FROM alpine\n"), 0o644))

	cleanup, err := applyRecipeIgnore(outputDir, dockerfile, &builderv0.DockerBuildRecipe{Dockerignore: "dockerignore"})
	require.NoError(t, err)

	// buildx discovers "<dockerfile>.dockerignore"; that sibling must now exist
	// with the recipe's ignore content.
	staged, err := os.ReadFile(dockerfile + ".dockerignore")
	require.NoError(t, err)
	require.Equal(t, "code/node_modules\n", string(staged))

	cleanup()
	_, err = os.Stat(dockerfile + ".dockerignore")
	require.True(t, os.IsNotExist(err), "staged ignore must be cleaned up")
}

func TestApplyRecipeIgnoreNoIgnoreIsNoOp(t *testing.T) {
	outputDir := t.TempDir()
	dockerfile := filepath.Join(outputDir, "Dockerfile")
	cleanup, err := applyRecipeIgnore(outputDir, dockerfile, &builderv0.DockerBuildRecipe{})
	require.NoError(t, err)
	cleanup()
	_, err = os.Stat(dockerfile + ".dockerignore")
	require.True(t, os.IsNotExist(err))
}

func TestApplyRecipeIgnoreRefusesToClobber(t *testing.T) {
	outputDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outputDir, "dockerignore"), []byte("x\n"), 0o644))
	dockerfile := filepath.Join(outputDir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile+".dockerignore", []byte("existing\n"), 0o644))

	_, err := applyRecipeIgnore(outputDir, dockerfile, &builderv0.DockerBuildRecipe{Dockerignore: "dockerignore"})
	require.Error(t, err)
	// The pre-existing sibling is left intact.
	data, readErr := os.ReadFile(dockerfile + ".dockerignore")
	require.NoError(t, readErr)
	require.Equal(t, "existing\n", string(data))
}

func TestReadPushedImageDigest(t *testing.T) {
	metadata := filepath.Join(t.TempDir(), "meta.json")
	digest := "sha256:" + strings.Repeat("a", 64)
	require.NoError(t, os.WriteFile(metadata, []byte(`{"containerimage.digest":"`+digest+`","image.name":"repo/app:v1"}`), 0o644))
	got, err := readPushedImageDigest(metadata)
	require.NoError(t, err)
	require.Equal(t, digest, got)

	bad := filepath.Join(t.TempDir(), "meta.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"image.name":"repo/app:v1"}`), 0o644))
	_, err = readPushedImageDigest(bad)
	require.Error(t, err)
}

func TestBuildRecipeOutputDirectoryIsAbsoluteAndDoesNotCreate(t *testing.T) {
	serviceDir := t.TempDir()
	outputDir, err := buildRecipeOutputDirectory(serviceDir)
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(outputDir))
	require.Equal(t, filepath.Join(serviceDir, buildRecipeDir), outputDir)
	// The CLI must not create builder/ — a legacy agent that ignores the field
	// must not have an empty directory left behind.
	_, statErr := os.Stat(outputDir)
	require.True(t, os.IsNotExist(statErr))
}
