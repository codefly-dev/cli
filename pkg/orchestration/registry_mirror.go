package orchestration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
)

// REGISTRY MIRRORS: READING A BASE IMAGE FROM SOMEWHERE ELSE.
//
// Every agent's Dockerfile names its base generically — `FROM debian:trixie-slim`
// — because an agent is written once and built into every cell. The registry
// that reference resolves against is therefore Docker Hub, anonymously, for
// every service of every environment, and Docker Hub throttles anonymous reads
// per IP: a render that resolves and pulls a dozen bases shares one quota with
// everything else leaving the same address, and a `429` fails it.
//
// An environment answers that with `registry.mirrors`, a map from a registry's
// canonical host to where this environment reads it from instead (typically an
// Artifact Registry remote repository). It is applied in the two places a build
// reaches a base image, and nowhere else:
//
//   - the BuildKit builder the CLI provisions is created with a buildkitd
//     registry configuration naming the mirror, so `FROM debian:trixie-slim`
//     pulls through it with no Dockerfile naming a cell's registry;
//   - the base-image resolution the reuse decision performs (baseImageDigests)
//     reads the manifest from the mirrored reference.
//
// It is a read-side substitution. Nothing is pushed to a mirror, the recipe is
// unchanged, and — because a mirror serves the same content as what it mirrors —
// the resolved digest is the same either way, so an image built through a mirror
// is not a changed input: imageInputs.Bases records the reference the recipe
// names with the digest that was resolved, never the mirror it came from.

// registryMirrors is the environment's declared mirrors, or nothing. An
// environment is absent from a flow that has none (an in-process build with no
// selected environment), and a mirror is a property of the environment alone.
func registryMirrors(env *environments.Environment) map[string]string {
	if env == nil || env.Registry == nil {
		return nil
	}
	return env.Registry.Mirrors
}

// mirroredReference is the reference to read `reference` from: the same
// repository and tag under the mirror declared for its registry, or the
// reference itself when no mirror is declared for it.
//
// A mirror of Docker Hub holds the upstream repository path verbatim, which for
// an unqualified reference is the `library` namespace Docker resolves it to —
// so `debian:trixie-slim` is read as `<mirror>/library/debian:trixie-slim`,
// which is what an Artifact Registry remote repository (and a pull-through
// registry generally) serves it at.
func mirroredReference(reference string, mirrors map[string]string) string {
	if len(mirrors) == 0 {
		return reference
	}
	host, repository := referenceRegistry(reference)
	mirror := mirrors[host]
	if mirror == "" {
		return reference
	}
	return mirror + "/" + repository
}

// referenceRegistry splits an image reference into the canonical host of the
// registry it resolves against and the repository (with its tag or digest)
// within it, by Docker's own rule: a first path component carrying a dot or a
// port, or spelled `localhost`, is a registry host, and anything else is a
// Docker Hub repository — one with no path component at all being in Hub's
// `library` namespace.
func referenceRegistry(reference string) (string, string) {
	first, rest, qualified := strings.Cut(reference, "/")
	if qualified && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return environments.RegistryMirrorHost(first), rest
	}
	if !qualified {
		return environments.DockerHubRegistry, "library/" + reference
	}
	return environments.DockerHubRegistry, reference
}

// buildkitRegistryConfig renders the mirrors as the buildkitd configuration a
// builder is created with. Empty when nothing is mirrored, which is what makes
// the unmirrored builder the one every environment without mirrors shares.
//
// The declared mirror may carry a repository path (an Artifact Registry remote
// repository is `<host>/<project>/<repo>`); BuildKit splits a mirror entry into
// its host and path and prefixes the upstream repository with the path, which is
// how such a repository is read.
func buildkitRegistryConfig(mirrors map[string]string) string {
	if len(mirrors) == 0 {
		return ""
	}
	var config strings.Builder
	for _, host := range slices.Sorted(maps.Keys(mirrors)) {
		mirror := mirrors[host]
		if mirror == "" {
			continue
		}
		// Both sides are held to a registry reference by
		// EnvironmentRegistry.Validate, so Go quoting is TOML quoting here.
		fmt.Fprintf(&config, "[registry.%q]\n  mirrors = [%q]\n", host, mirror)
	}
	return config.String()
}

// buildxBuilderFor names the builder that carries this registry configuration.
//
// A builder's buildkitd configuration is fixed when it is created and is not
// readable back out of it, so the only way a build can be sure the builder it
// runs on mirrors what this environment declares is for the configuration to
// decide the builder's name. An environment with no mirrors keeps the plain
// `codefly` builder every build has always shared; each distinct mirror set
// gets its own, so two environments rendered from one workspace never hand each
// other a builder configured for the other's registry, and a changed mirror
// provisions a new builder instead of silently reusing one that still reads
// from the old place.
func buildxBuilderFor(mirrors map[string]string) string {
	config := buildkitRegistryConfig(mirrors)
	if config == "" {
		return buildxBuilderName
	}
	sum := sha256.Sum256([]byte(config))
	return buildxBuilderName + "-" + hex.EncodeToString(sum[:])[:12]
}

// stageBuildkitConfig writes the registry configuration for `docker buildx
// create --config` and returns its path, or an empty path when there is nothing
// to configure. buildx reads the file at creation and keeps its contents in the
// builder, so the file itself does not outlive the call.
func stageBuildkitConfig(mirrors map[string]string) (string, func(), error) {
	config := buildkitRegistryConfig(mirrors)
	if config == "" {
		return "", func() {}, nil
	}
	file, err := os.CreateTemp("", "codefly-buildkitd-*.toml")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	remove := func() { _ = os.Remove(path) }
	if _, err = file.WriteString(config); err != nil {
		_ = file.Close()
		remove()
		return "", func() {}, err
	}
	if err = file.Close(); err != nil {
		remove()
		return "", func() {}, err
	}
	return path, remove, nil
}
