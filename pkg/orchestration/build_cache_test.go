package orchestration

import (
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBuildCacheIsOwnedByFlowAndScopedByServiceAndRecipe(t *testing.T) {
	cache := &builderv0.BuildCacheOptions{Backend: "registry", Scope: "workspace/protected", Exports: []string{"ghcr.io/org/cache"}}
	flow := &Flow{world: &World{}}
	flow.WithBuildCache(cache)
	cache.Exports[0] = "ghcr.io/different/cache"
	require.Equal(t, "ghcr.io/org/cache", flow.world.BuildCache.Exports[0])
	a := scopedBuildCache(flow.world.BuildCache, "module/a", "app")
	b := scopedBuildCache(flow.world.BuildCache, "module/b", "app")
	migration := scopedBuildCache(flow.world.BuildCache, "module/a", "migration")
	require.NotEqual(t, a.Scope, b.Scope)
	require.NotEqual(t, a.Scope, migration.Scope)
	require.Equal(t, "workspace/protected", flow.world.BuildCache.Scope)
}

func TestCachedBuildxArgsMatchExecutedPlatformsAndPreserveOutputIdentity(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{Name: "app", Image: "ghcr.io/org/app:v1", Platforms: []string{"linux/amd64", "linux/arm64"}, Target: "runtime", BuildArgs: map[string]string{"PUBLIC": "value"}}
	cache := &builderv0.BuildCacheOptions{Backend: "registry", Scope: "workspace/service/app", Imports: []string{"ghcr.io/org/cache"}, Exports: []string{"ghcr.io/org/cache"}}
	for _, push := range []bool{false, true} {
		args, err := cachedBuildxArgs(recipe, "/staged/Dockerfile", "/staged/context", push, push, "/tmp/metadata", "remote", cache)
		require.NoError(t, err)
		require.Equal(t, "/staged/context", args[len(args)-1])
		require.Contains(t, args, "--metadata-file")
		require.Contains(t, args, "/tmp/metadata")
		require.Contains(t, args, "runtime")
		require.Contains(t, args, "PUBLIC=value")
		require.Contains(t, args, recipe.Image)
		require.Contains(t, args, "--pull")
		require.Contains(t, args, "--progress")
		require.Contains(t, args, "remote")
		count := 0
		for _, arg := range args {
			if arg == "--cache-to" {
				count++
			}
		}
		if push {
			require.Equal(t, 2, count)
			require.Contains(t, args, "linux/amd64,linux/arm64")
			require.Contains(t, args, "--push")
		} else {
			require.Equal(t, 1, count)
			require.Contains(t, args, "linux/amd64")
			require.Contains(t, args, "--load")
		}
	}
	cache.Exports = nil
	args, err := cachedBuildxArgs(recipe, "/staged/Dockerfile", "/staged/context", false, false, "", "", cache)
	require.NoError(t, err)
	require.NotContains(t, args, "--cache-to")
	cache.Backend = "unsupported"
	_, err = cachedBuildxArgs(recipe, "/staged/Dockerfile", "/staged/context", false, false, "", "", cache)
	require.ErrorContains(t, err, "unsupported")
}
