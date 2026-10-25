package orchestration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

// A mirror is declared for a registry, and the reference a recipe names decides
// which registry that is — by Docker's own rule, which puts an unqualified
// reference in Docker Hub's `library` namespace. That namespace is where a
// pull-through repository serves it, so it has to be in the mirrored reference.
func TestAReferenceIsMirroredByTheRegistryItResolvesAgainst(t *testing.T) {
	mirrors := map[string]string{
		"docker.io": "us-east1-docker.pkg.dev/example/dockerhub",
		"ghcr.io":   "us-east1-docker.pkg.dev/example/ghcr",
	}
	for _, testCase := range []struct{ reference, mirrored string }{
		{"debian:trixie-slim", "us-east1-docker.pkg.dev/example/dockerhub/library/debian:trixie-slim"},
		{"golang:1.25", "us-east1-docker.pkg.dev/example/dockerhub/library/golang:1.25"},
		{"bitnami/postgresql:17", "us-east1-docker.pkg.dev/example/dockerhub/bitnami/postgresql:17"},
		// index.docker.io is the one alias the reference parser folds into
		// docker.io, so a mirror declared there covers it.
		{"index.docker.io/library/debian:trixie-slim", "us-east1-docker.pkg.dev/example/dockerhub/library/debian:trixie-slim"},
		// registry-1.docker.io is not folded — by the parser or by BuildKit —
		// so the docker.io mirror does not cover it, and the resolution must
		// not pretend otherwise: a read redirected here while the build pulls
		// direct is exactly the skew this keeps out.
		{"registry-1.docker.io/library/debian:trixie-slim", "registry-1.docker.io/library/debian:trixie-slim"},
		// A digest-pinned reference keeps its digest through the mirror.
		{"debian@sha256:" + strings.Repeat("a", 64), "us-east1-docker.pkg.dev/example/dockerhub/library/debian@sha256:" + strings.Repeat("a", 64)},
		// A reference the parser refuses (an uppercase repository) is read
		// where it is named, and fails there with the registry's own error.
		{"Debian:trixie-slim", "Debian:trixie-slim"},
		{"ghcr.io/codefly-dev/toolbox:v1", "us-east1-docker.pkg.dev/example/ghcr/codefly-dev/toolbox:v1"},
		// A registry with no declared mirror is read where it is named, and so
		// is a host-qualified reference nobody mirrored.
		{"quay.io/prometheus/node-exporter:v1", "quay.io/prometheus/node-exporter:v1"},
		{"localhost:5001/app:dev", "localhost:5001/app:dev"},
		{"us-east1-docker.pkg.dev/example/app/api:v1", "us-east1-docker.pkg.dev/example/app/api:v1"},
	} {
		t.Run(testCase.reference, func(t *testing.T) {
			require.Equal(t, testCase.mirrored, mirroredReference(testCase.reference, mirrors))
			// With nothing declared, every reference is read where it is named.
			require.Equal(t, testCase.reference, mirroredReference(testCase.reference, nil))
		})
	}
	// Declared under its own spelling, registry-1.docker.io is a registry like
	// any other — which is the way to cover a Dockerfile that spells it.
	require.Equal(t, "mirror.example/hub-api/library/debian:trixie-slim",
		mirroredReference("registry-1.docker.io/library/debian:trixie-slim",
			map[string]string{"registry-1.docker.io": "mirror.example/hub-api"}))
}

// The builder carries the mirror in its buildkitd configuration, which is what
// makes `FROM debian:…` pull through the mirror with no Dockerfile naming it.
// A builder's configuration cannot be read back out of it, so the configuration
// names the builder: an environment with no mirrors keeps the shared `codefly`
// builder, and each mirror set gets one of its own.
func TestTheBuilderIsNamedForTheRegistryConfigurationItCarries(t *testing.T) {
	hub := map[string]string{"docker.io": "us-east1-docker.pkg.dev/example/dockerhub"}
	other := map[string]string{"docker.io": "europe-west1-docker.pkg.dev/example/dockerhub"}

	require.Equal(t, "[registry.\"docker.io\"]\n  mirrors = [\"us-east1-docker.pkg.dev/example/dockerhub\"]\n", buildkitRegistryConfig(hub))
	require.Empty(t, buildkitRegistryConfig(nil))
	// Two registries render in a stable order, so the builder's name does not
	// depend on map iteration.
	both := map[string]string{"ghcr.io": "mirror.example/ghcr", "docker.io": "mirror.example/hub"}
	require.Equal(t, "[registry.\"docker.io\"]\n  mirrors = [\"mirror.example/hub\"]\n[registry.\"ghcr.io\"]\n  mirrors = [\"mirror.example/ghcr\"]\n", buildkitRegistryConfig(both))

	require.Equal(t, buildxBuilderName, buildxBuilderFor(nil), "nothing mirrored keeps the builder every build has always shared")
	require.Equal(t, buildxBuilderName, buildxBuilderFor(map[string]string{}))
	require.NotEqual(t, buildxBuilderName, buildxBuilderFor(hub))
	require.Equal(t, buildxBuilderFor(hub), buildxBuilderFor(hub), "the name is the configuration, so it is stable")
	require.NotEqual(t, buildxBuilderFor(hub), buildxBuilderFor(other), "a changed mirror provisions its own builder rather than reusing one configured for the old place")
	require.True(t, strings.HasPrefix(buildxBuilderFor(hub), buildxBuilderName+"-"))
}

// The configuration a builder is created with is the one the mirrors describe,
// written where `docker buildx create --config` reads it.
func TestTheStagedBuildkitConfigIsTheDeclaredMirror(t *testing.T) {
	path, remove, err := stageBuildkitConfig(map[string]string{"docker.io": "mirror.example/hub"})
	require.NoError(t, err)
	content, err := os.ReadFile(path) //nolint:gosec // the path is this test's own temporary file
	require.NoError(t, err)
	require.Equal(t, "[registry.\"docker.io\"]\n  mirrors = [\"mirror.example/hub\"]\n", string(content))
	remove()
	require.NoFileExists(t, path)

	// Nothing mirrored stages no file, which is what leaves the builder on the
	// host's own registry configuration.
	path, remove, err = stageBuildkitConfig(nil)
	require.NoError(t, err)
	require.Empty(t, path)
	remove()
}

func TestRegistryMirrorsComeFromTheEnvironmentOrNowhere(t *testing.T) {
	require.Nil(t, registryMirrors(nil))
	require.Nil(t, registryMirrors(&environments.Environment{}))
	declared := map[string]string{"docker.io": "mirror.example/hub"}
	require.Equal(t, declared, registryMirrors(&environments.Environment{Registry: &environments.EnvironmentRegistry{URL: "mirror.example/app", Mirrors: declared}}))
}

// The point of the mirror is that no read of a base image reaches the registry
// the recipe names. Nothing in the identity path may therefore ask Docker Hub
// for a reference an environment mirrors.
func TestNoBaseImageReadReachesTheMirroredRegistry(t *testing.T) {
	fixture := newIdentityFixture(t, "FROM debian:trixie-slim\nCOPY code /app\n", "Dockerfile")
	fixture.mirrors = map[string]string{"docker.io": "us-east1-docker.pkg.dev/example/dockerhub"}
	previous := resolveImageManifestDigest
	t.Cleanup(func() { resolveImageManifestDigest = previous })
	var asked []string
	resolveImageManifestDigest = func(_ context.Context, reference, _ string) (string, error) {
		asked = append(asked, reference)
		// The blocked host: Docker Hub answers nothing at all here, so a read
		// that reached it would fail the recipe rather than pass unnoticed.
		if host, _, _ := environments.ReferenceRegistry(reference); host == environments.DockerHubRegistry {
			return "", fmt.Errorf("dial registry-1.docker.io: blocked")
		}
		return "sha256:" + strings.Repeat("a", 64), nil
	}

	key := fixture.key(t, []string{"Dockerfile"})
	require.Equal(t, []string{"us-east1-docker.pkg.dev/example/dockerhub/library/debian:trixie-slim"}, asked)
	require.NotEmpty(t, key)
}

// A mirror serves the same content as what it mirrors, so an image built
// through one is not an image built from different inputs. The resolved digest
// is the whole of what the base contributes to the identity; where it was read
// from contributes nothing.
func TestAMirrorDoesNotChangeTheImageIdentity(t *testing.T) {
	previous := resolveImageManifestDigest
	t.Cleanup(func() { resolveImageManifestDigest = previous })
	digest := "sha256:" + strings.Repeat("c", 64)
	resolveImageManifestDigest = func(context.Context, string, string) (string, error) { return digest, nil }

	direct := newIdentityFixture(t, "FROM debian:trixie-slim\nCOPY code /app\n", "Dockerfile")
	through := newIdentityFixture(t, "FROM debian:trixie-slim\nCOPY code /app\n", "Dockerfile")
	through.mirrors = map[string]string{"docker.io": "us-east1-docker.pkg.dev/example/dockerhub"}

	require.Equal(t, direct.key(t, []string{"Dockerfile"}), through.key(t, []string{"Dockerfile"}),
		"the same base digest is the same input whichever registry served it")

	// And the base is still bound: a moved base is still a changed input,
	// mirror or no mirror.
	moved := "sha256:" + strings.Repeat("d", 64)
	before := through.key(t, []string{"Dockerfile"})
	digest = moved
	require.NotEqual(t, before, through.key(t, []string{"Dockerfile"}))
}

// A digest IS the resolution, so a reference already pinned by one is bound
// without asking any registry. It matters beyond the saved round trip: an
// unresolvable reference declines its whole recipe, so a throttled registry
// would otherwise rebuild every image whose inputs provably did not move.
func TestADigestPinnedBaseImageIsBoundWithoutARegistryRead(t *testing.T) {
	pinned := "sha256:" + strings.Repeat("e", 64)
	fixture := newIdentityFixture(t, "FROM debian@"+pinned+"\nCOPY code /app\n", "Dockerfile")
	previous := resolveImageManifestDigest
	t.Cleanup(func() { resolveImageManifestDigest = previous })
	resolveImageManifestDigest = func(_ context.Context, reference, _ string) (string, error) {
		return "", fmt.Errorf("no registry may be read for %s", reference)
	}

	key := fixture.key(t, []string{"Dockerfile"})
	require.NotEmpty(t, key)

	// It is bound in the spelling a resolved reference produces, so an identity
	// recorded before this short circuit existed still matches.
	bases, err := baseImageDigests(t.Context(), filepath.Join(fixture.output, "Dockerfile"), "", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"debian@" + pinned + "@" + pinned}, bases)

	// A tag beside it is still resolved: only the pinned reference is answered
	// from itself.
	tagged := newIdentityFixture(t, "FROM debian@"+pinned+" AS base\nFROM alpine:3.21\nCOPY --from=base /app /app\n", "Dockerfile")
	_, err = baseImageDigests(t.Context(), filepath.Join(tagged.output, "Dockerfile"), "", nil)
	require.ErrorContains(t, err, "resolve base image alpine:3.21")
}

// A mirror is written into the builder's buildkitd.toml and prefixed onto an
// image reference, so what it may contain is held to a registry reference and
// checked where the environment is read, not where it is used.
func TestADeclaredMirrorIsHeldToARegistryReference(t *testing.T) {
	valid := func(host, mirror string) error {
		return (&environments.EnvironmentRegistry{URL: "example.test/app", Mirrors: map[string]string{host: mirror}}).Validate()
	}
	require.NoError(t, valid("docker.io", "us-east1-docker.pkg.dev/example/dockerhub"))
	require.NoError(t, valid("localhost:5001", "mirror.example"))
	require.NoError(t, (&environments.EnvironmentRegistry{URL: "example.test/app"}).Validate())
	require.NoError(t, (*environments.EnvironmentRegistry)(nil).Validate())

	require.ErrorContains(t, valid("docker.io", ""), "cannot be empty")
	require.ErrorContains(t, valid("docker.io", "https://mirror.example"), "carries a scheme")
	require.ErrorContains(t, valid("docker.io", "mirror.example/hub:v1"), "carries a tag")
	require.ErrorContains(t, valid("docker.io", "mirror.example/hub@sha256:abc"), "carries a digest")
	require.ErrorContains(t, valid("docker.io", "mirror.example/\"]\ninsecure = true"), "is not a registry reference")
	require.ErrorContains(t, valid("docker.io/library", "mirror.example"), "names a repository path")
	// index.docker.io is folded into docker.io by the reference parser, so no
	// reference ever keys a mirror on it; a bare word is a Hub repository, not
	// a host. registry-1.docker.io is folded by nothing — neither the parser
	// nor BuildKit — so it is a key of its own rather than a rejected alias.
	require.ErrorContains(t, valid("index.docker.io", "mirror.example"), `declare the mirror under "docker.io"`)
	require.ErrorContains(t, valid("myregistry", "mirror.example"), `declare the mirror under "docker.io"`)
	require.NoError(t, valid("registry-1.docker.io", "mirror.example"))
}
