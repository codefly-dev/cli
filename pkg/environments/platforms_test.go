package environments

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func hostIs(architecture string) func() (string, error) {
	return func() (string, error) { return architecture, nil }
}

func TestImagePlatformsAreTheDeclaredArchitectures(t *testing.T) {
	env := &Environment{Name: "staging", Cluster: &EnvironmentCluster{Kind: "gke", Architectures: []string{"amd64"}}}
	platforms, err := env.ImagePlatforms(hostIs("arm64"))
	require.NoError(t, err)
	require.Equal(t, []string{"linux/amd64"}, platforms, "a declaration wins over the building host")

	env.Cluster.Architectures = []string{"amd64", "arm64"}
	platforms, err = env.ImagePlatforms(hostIs("arm64"))
	require.NoError(t, err)
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, platforms)
}

func TestImagePlatformsOfALocalClusterAreTheEngines(t *testing.T) {
	for _, kind := range []string{"k3d", "kind", "minikube"} {
		env := &Environment{Name: "local", Cluster: &EnvironmentCluster{Kind: kind}}
		platforms, err := env.ImagePlatforms(hostIs("arm64"))
		require.NoError(t, err, kind)
		require.Equal(t, []string{"linux/arm64"}, platforms, kind)
	}
	env := &Environment{Name: "local", Cluster: &EnvironmentCluster{Kind: "k3d"}}
	_, err := env.ImagePlatforms(func() (string, error) { return "", errors.New("engine unreachable") })
	require.ErrorContains(t, err, "engine unreachable")
}

func TestImagePlatformsRefuseAnUndeclaredCell(t *testing.T) {
	_, err := (&Environment{Name: "staging", Cluster: &EnvironmentCluster{Kind: "gke"}}).ImagePlatforms(hostIs("amd64"))
	require.ErrorContains(t, err, "cluster.architectures")
	_, err = (&Environment{Name: "production"}).ImagePlatforms(hostIs("amd64"))
	require.ErrorContains(t, err, "declares no cluster")
}

func TestImagePlatformsRejectMalformedDeclarations(t *testing.T) {
	for name, architectures := range map[string][]string{
		"platform form": {"linux/amd64"},
		"not an arch":   {"x86_64"},
		"duplicate":     {"amd64", "amd64"},
	} {
		env := &Environment{Name: "staging", Cluster: &EnvironmentCluster{Kind: "gke", Architectures: architectures}}
		_, err := env.ImagePlatforms(hostIs("amd64"))
		require.Error(t, err, name)
	}
}
