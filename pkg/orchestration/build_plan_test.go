package orchestration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func TestBuildxArgsPushIsMultiArchManifestList(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64", "linux/arm64"},
		Target:    "final",
		BuildArgs: map[string]string{"VERSION": "1", "COMMIT": "abc"},
	}
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", true)
	joined := strings.Join(args, " ")

	require.Equal(t, []string{"buildx", "build"}, args[:2])
	require.Contains(t, joined, "--platform linux/amd64,linux/arm64")
	require.Contains(t, joined, "--push")
	require.NotContains(t, joined, "--load")
	require.Contains(t, joined, "--target final")
	// Build args are emitted in sorted key order for a stable command.
	require.Contains(t, joined, "--build-arg COMMIT=abc --build-arg VERSION=1")
	require.Equal(t, []string{"-t", "repo/app:v1", "-f", "/svc/builder/Dockerfile", "/svc"}, args[len(args)-5:])
}

func TestBuildxArgsLocalBuildIsSinglePlatformLoad(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", false)
	joined := strings.Join(args, " ")

	// A local load cannot materialize a multi-platform manifest list.
	require.Contains(t, joined, "--platform linux/amd64")
	require.NotContains(t, joined, "linux/amd64,linux/arm64")
	require.Contains(t, joined, "--load")
	require.NotContains(t, joined, "--push")
}

func TestBuildRecipeOutputDirectoryIsAbsoluteBuilderDir(t *testing.T) {
	serviceDir := t.TempDir()
	outputDir, err := buildRecipeOutputDirectory(serviceDir)
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(outputDir))
	require.Equal(t, filepath.Join(serviceDir, buildRecipeDir), outputDir)
	info, err := os.Stat(outputDir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}
