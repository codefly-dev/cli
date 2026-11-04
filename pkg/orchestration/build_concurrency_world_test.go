package orchestration

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/pkg/environments"
)

// Every service's build narrows its recipe to the environment's platforms, so
// once several builds run at once this memoization is read and written by all
// of them. Run under -race, this is the test that would have caught it.
func TestImagePlatformsResolvesOnceUnderConcurrentBuilds(t *testing.T) {
	var reads int
	var mu sync.Mutex
	world := &World{
		Env: &environments.Environment{
			Name:    "blue",
			Cluster: &environments.EnvironmentCluster{Kind: "kind"},
		},
		hostArchitecture: func() (string, error) {
			mu.Lock()
			defer mu.Unlock()
			reads++
			return "arm64", nil
		},
	}

	const builds = 16
	results := make([][]string, builds)
	errs := make([]error, builds)
	var waiting sync.WaitGroup
	for index := range builds {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			results[index], errs[index] = world.imagePlatforms()
		}()
	}
	waiting.Wait()

	for index := range builds {
		require.NoError(t, errs[index])
		require.Equal(t, []string{"linux/arm64"}, results[index])
	}
	// Resolution runs `docker info`; sixteen builds must not run it sixteen
	// times to learn the same answer.
	require.Equal(t, 1, reads)
}

func TestConcurrentBuildsAreCountedForTheWholePhase(t *testing.T) {
	world := &World{}
	require.False(t, world.buildsConcurrently(), "a flow that is not building shares no terminal")

	restore := world.withConcurrentBuilds(4)
	require.True(t, world.buildsConcurrently())
	restore()
	require.False(t, world.buildsConcurrently())

	// One build has the terminal to itself and keeps streaming, as it always
	// has.
	restore = world.withConcurrentBuilds(1)
	require.False(t, world.buildsConcurrently())
	restore()
}

// A nil world is what a flow built by a test without one looks like; it must
// not make a build panic on the way to docker.
func TestConcurrentBuildMarkerToleratesNoWorld(t *testing.T) {
	var world *World
	require.False(t, world.buildsConcurrently())
	require.NotPanics(t, func() { world.withConcurrentBuilds(4)() })
}
