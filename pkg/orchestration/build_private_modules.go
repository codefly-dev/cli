package orchestration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// netrcSecretID is the BuildKit secret id an agent's Dockerfile mounts on
	// its dependency download (`RUN --mount=type=secret,id=netrc,…`). It is
	// part of the recipe contract between the CLI and every Go agent.
	netrcSecretID = "netrc"
	// goPrivateBuildArg is the build argument carrying the host's GOPRIVATE
	// into the builder stage, where the Dockerfile declares it as `ARG`.
	goPrivateBuildArg = "GOPRIVATE"
	// buildNetrcEnv names the netrc file to mount, for a CI job that writes one
	// from its token and must not rely on the runner's home directory.
	buildNetrcEnv = "CODEFLY_BUILD_NETRC"
	// goNetrcEnv is the Go toolchain's own override of the netrc location; the
	// CLI honours it so a host whose `go` already reaches private modules
	// builds images the same way.
	goNetrcEnv = "NETRC"
	// netrcFileName is the file git and the go tool read from the home
	// directory when nothing overrides it.
	netrcFileName = ".netrc"
)

// privateModuleBuild is what an image build needs to fetch Go modules the
// public proxy does not serve: the module paths to fetch directly (GOPRIVATE)
// and the credential file to authenticate with. Both are properties of the
// machine running the build — the same service builds through an authenticated
// GOPROXY with neither — so they come from the host environment, exactly as the
// go tool takes them on the host, and never from the recipe or the manifest.
// The credential travels as a BuildKit secret: mounted for one RUN, never an
// argument, an ENV or a layer, so it is absent from the image and its cache.
type privateModuleBuild struct {
	// GoPrivate is the host's GOPRIVATE, empty when unset.
	GoPrivate string
	// Netrc is the netrc file mounted as the "netrc" secret, empty when none.
	Netrc string
}

// resolvePrivateModuleBuild reads the private-module inputs of a build from the
// host. GOPRIVATE is taken as-is. The netrc file is the first of
// CODEFLY_BUILD_NETRC, NETRC and $HOME/.netrc: the explicit CODEFLY_BUILD_NETRC
// must exist, since a CI job that set it and gets an unauthenticated build
// would otherwise only learn of the typo from git's error deep in the build
// log; the other two are what git and go read opportunistically, so a missing
// one means "no credential" as it does for them.
func resolvePrivateModuleBuild(lookupEnv func(string) (string, bool), homeDir func() (string, error)) (privateModuleBuild, error) {
	var build privateModuleBuild
	if value, ok := lookupEnv(goPrivateBuildArg); ok {
		build.GoPrivate = strings.TrimSpace(value)
	}
	if explicit, ok := lookupEnv(buildNetrcEnv); ok && explicit != "" {
		if err := checkNetrcFile(explicit); err != nil {
			return privateModuleBuild{}, fmt.Errorf("%s: %w", buildNetrcEnv, err)
		}
		build.Netrc = explicit
		return build, nil
	}
	candidates := make([]string, 0, 2)
	if value, ok := lookupEnv(goNetrcEnv); ok && value != "" {
		candidates = append(candidates, value)
	}
	if home, err := homeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, netrcFileName))
	}
	for _, candidate := range candidates {
		if err := checkNetrcFile(candidate); err == nil {
			build.Netrc = candidate
			break
		}
	}
	return build, nil
}

// checkNetrcFile accepts a readable regular file whose path buildx can carry in
// a `--secret` flag. buildx parses that flag as CSV, so a comma in the path
// would be read as a second option; the build would then fail on the flag
// rather than on the credential, which is the one outcome this must prevent.
func checkNetrcFile(path string) error {
	if strings.Contains(path, ",") {
		return fmt.Errorf("netrc path %q must not contain a comma", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("netrc file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("netrc file %q is not a regular file", path)
	}
	return nil
}

// buildxArgs renders the private-module inputs as docker buildx flags. The
// secret carries only a path: BuildKit reads the file itself and exposes it to
// the RUN that mounts it, so the credential never appears in the argv, in a
// layer or in the build cache. A recipe that declares GOPRIVATE itself keeps
// its own value — a durable declaration beats the environment of one machine.
func (p privateModuleBuild) buildxArgs(recipeBuildArgs map[string]string) []string {
	var args []string
	if _, declared := recipeBuildArgs[goPrivateBuildArg]; p.GoPrivate != "" && !declared {
		args = append(args, "--build-arg", goPrivateBuildArg+"="+p.GoPrivate)
	}
	if p.Netrc != "" {
		args = append(args, "--secret", "id="+netrcSecretID+",src="+p.Netrc)
	}
	return args
}
