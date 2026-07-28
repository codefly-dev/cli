package companion

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveDockerPlatformsDefaultsToHostLinuxArchitecture(t *testing.T) {
	platforms, err := resolveDockerPlatforms("")
	require.NoError(t, err)
	require.Equal(t, []dockerPlatform{{
		Value: "linux/" + runtime.GOARCH,
		Arch:  runtime.GOARCH,
	}}, platforms)
}

func TestResolveDockerPlatformsSupportsMultiArchitectureManifest(t *testing.T) {
	platforms, err := resolveDockerPlatforms(" linux/amd64, linux/arm64 ")
	require.NoError(t, err)
	require.Equal(t, []dockerPlatform{
		{Value: "linux/amd64", Arch: "amd64"},
		{Value: "linux/arm64", Arch: "arm64"},
	}, platforms)
}

func TestResolveDockerPlatformsRejectsInvalidAndDuplicateTargets(t *testing.T) {
	for _, value := range []string{
		"darwin/arm64",
		"linux/386",
		"linux/amd64/v2",
		"linux/amd64,linux/amd64",
		",",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := resolveDockerPlatforms(value)
			require.Error(t, err)
		})
	}
}
