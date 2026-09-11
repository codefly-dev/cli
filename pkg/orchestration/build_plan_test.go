package orchestration

import (
	"os"
	"path/filepath"
	"slices"
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
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", true, true, "/tmp/meta.json", "")
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
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", false, false, "", "")
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

func TestBuildxArgsCallerBuilderWinsOverContainerBuilder(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}
	// A caller-provided builder (e.g. a native amd64 buildkit) is authoritative,
	// even for a multi-arch push that would otherwise select "codefly".
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", true, true, "/tmp/meta.json", "amd64-remote")
	joined := strings.Join(args, " ")

	require.Contains(t, joined, "--builder amd64-remote")
	require.NotContains(t, joined, "--builder codefly")
	require.Contains(t, joined, "--push")
}

func TestBuildxArgsSinglePlatformPushOnCallerBuilder(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64"},
	}
	// The #501 targeted amd64 fix: one platform, pushed on a native builder so
	// no local QEMU is involved. Single-platform builds are not "multiArch".
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", true, false, "/tmp/meta.json", "amd64-remote")
	joined := strings.Join(args, " ")

	require.Contains(t, joined, "--builder amd64-remote")
	require.Contains(t, joined, "--platform linux/amd64")
	require.Contains(t, joined, "--push")
	require.NotContains(t, joined, "--load")
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

func TestBuilderHasHostNetwork(t *testing.T) {
	// A builder created with --driver-opt network=host reports it under Driver
	// Options; a pre-fix builder omits the line entirely and must read as stale
	// so the caller refuses to reuse it instead of stalling on tailnet DNS.
	ready := []byte("Name:          codefly\nDriver:        docker-container\nNodes:\nName:                  codefly0\nDriver Options:        network=\"host\"\nStatus:                running\n")
	require.True(t, builderHasHostNetwork(ready))

	stale := []byte("Name:          codefly\nDriver:        docker-container\nNodes:\nName:                  codefly0\nStatus:                running\n")
	require.False(t, builderHasHostNetwork(stale))
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

func TestBuildCacheSeparatesServicesRecipesAndWorkspaces(t *testing.T) {
	cache := &builderv0.BuildCacheOptions{Backend: "registry", Scope: "protected/app", Imports: []string{"ghcr.io/org/cache"}, Exports: []string{"ghcr.io/org/cache"}, Mode: "max"}
	references := map[string]bool{}
	for _, identity := range [][3]string{
		{"workspace", "app/api", "app"}, {"workspace", "app/worker", "app"},
		{"workspace", "app/api", "migration"}, {"workspace", "app/api", ""},
		{"other", "app/api", "app"}, {"workspace/app", "api", "app"},
	} {
		scoped := scopedBuildCache(cache, identity[0], identity[1], identity[2])
		require.Equal(t, cache.Imports, scoped.Imports)
		require.Equal(t, cache.Exports, scoped.Exports)
		require.Equal(t, cache.Mode, scoped.Mode)
		recipe := &builderv0.DockerBuildRecipe{Image: "ghcr.io/org/app:v1", Platforms: []string{"linux/amd64"}}
		args, err := cachedBuildxArgs(recipe, "Dockerfile", ".", false, false, "", "", scoped)
		require.NoError(t, err)
		export := args[slices.Index(args, "--cache-to")+1]
		require.False(t, references[export], "cache export collides for %v", identity)
		references[export] = true
		recipe.Image = "ghcr.io/org/app:v2"
		again, err := cachedBuildxArgs(recipe, "Dockerfile", ".", false, false, "", "", scopedBuildCache(cache, identity[0], identity[1], identity[2]))
		require.NoError(t, err)
		require.Equal(t, export, again[slices.Index(again, "--cache-to")+1])
	}
	require.Equal(t, "protected/app", cache.Scope)
	require.Nil(t, scopedBuildCache(nil, "workspace", "app/api", "app"))
}
