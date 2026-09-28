package orchestration

import (
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

func platformBuilder(mode Mode, env *environments.Environment, host string) *Builder {
	return &Builder{world: &World{Mode: mode, Env: env, hostArchitecture: func() (string, error) { return host, nil }}}
}

func multiArchRecipe() *builderv0.DockerBuildRecipe {
	return &builderv0.DockerBuildRecipe{Name: "app", Image: "repo/app:v1", Platforms: []string{"linux/amd64", "linux/arm64"}}
}

// TestARenderBuildsOnlyTheCellsArchitectures is the defect this closes: a
// render for an all-amd64 cell built linux/arm64 too, under QEMU, which no node
// runs and which hung the render. The pushed recipe is narrowed to what the
// cell's nodes run, and the recipe's own declaration is left untouched.
func TestARenderBuildsOnlyTheCellsArchitectures(t *testing.T) {
	recipe := multiArchRecipe()
	staging := &environments.Environment{Name: "staging", Cluster: &environments.EnvironmentCluster{Kind: "gke", Architectures: []string{"amd64"}}}
	narrowed, err := platformBuilder(SnapshotMode, staging, "arm64").targetEnvironmentPlatforms(recipe)
	require.NoError(t, err)
	require.Equal(t, []string{"linux/amd64"}, narrowed.GetPlatforms())
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, recipe.GetPlatforms(), "the verified recipe is not mutated")
}

// TestALocalClusterRenderBuildsTheEnginesArchitecture keeps the local render
// native on either kind of machine: an amd64 CI runner and an Apple Silicon
// laptop each build for the k3d nodes they run.
func TestALocalClusterRenderBuildsTheEnginesArchitecture(t *testing.T) {
	local := &environments.Environment{Name: "local", Cluster: &environments.EnvironmentCluster{Kind: "k3d"}}
	for _, host := range []string{"amd64", "arm64"} {
		narrowed, err := platformBuilder(SnapshotMode, local, host).targetEnvironmentPlatforms(multiArchRecipe())
		require.NoError(t, err)
		require.Equal(t, []string{"linux/" + host}, narrowed.GetPlatforms())
	}
}

func TestARenderRefusesAnUndeclaredCell(t *testing.T) {
	for _, env := range []*environments.Environment{
		{Name: "staging", Cluster: &environments.EnvironmentCluster{Kind: "gke"}},
		{Name: "production"},
	} {
		for _, mode := range []Mode{SnapshotMode, DeployMode} {
			_, err := platformBuilder(mode, env, "amd64").targetEnvironmentPlatforms(multiArchRecipe())
			require.Error(t, err, "%s in %s", env.Name, mode)
		}
	}
}

func TestARenderRefusesAnArchitectureTheRecipeCannotBuild(t *testing.T) {
	cell := &environments.Environment{Name: "edge", Cluster: &environments.EnvironmentCluster{Kind: "eks", Architectures: []string{"arm64"}}}
	recipe := &builderv0.DockerBuildRecipe{Name: "app", Platforms: []string{"linux/amd64"}}
	_, err := platformBuilder(SnapshotMode, cell, "amd64").targetEnvironmentPlatforms(recipe)
	require.ErrorContains(t, err, "linux/arm64")
}

// TestAPortableBuildKeepsTheRecipesPlatforms: a pushed build for an environment
// that names no cluster is an artifact, not an image for a cell, so it builds
// every platform the recipe declares, which must still include amd64.
func TestAPortableBuildKeepsTheRecipesPlatforms(t *testing.T) {
	env := &environments.Environment{Name: "ci"}
	kept, err := platformBuilder(BuildMode, env, "arm64").targetEnvironmentPlatforms(multiArchRecipe())
	require.NoError(t, err)
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, kept.GetPlatforms())

	_, err = platformBuilder(BuildMode, env, "arm64").targetEnvironmentPlatforms(&builderv0.DockerBuildRecipe{Platforms: []string{"linux/arm64"}})
	require.ErrorContains(t, err, "linux/amd64")
}

func TestRecipeBuildsPlatformIgnoresVariants(t *testing.T) {
	require.True(t, recipeBuildsPlatform([]string{"linux/amd64/v2"}, "linux/amd64"))
	require.False(t, recipeBuildsPlatform([]string{"linux/arm64"}, "linux/amd64"))
}
