package orchestration

import (
	"sort"
	"strings"
)

// goPrivateBuildArg is the build argument carrying the host's GOPRIVATE into the
// builder stage, where the Dockerfile declares it as `ARG`.
const goPrivateBuildArg = "GOPRIVATE"

// privateModuleBuild is what an image build receives for Go modules the public
// proxy does not serve. It carries no credential: a recipe that downloads
// modules declares them (DockerBuildRecipe.go_module_downloads), the CLI fetches
// them on the host before any image build (goModulePrefetch), and the build
// reads them from the module proxy supplied as a named build context. Nothing
// secret is mounted into BuildKit, so nothing has to stay valid until a late
// build runs.
type privateModuleBuild struct {
	// GoPrivate is the host's GOPRIVATE, empty when unset. It names module
	// paths, not credentials.
	GoPrivate string
	// Proxies maps each Go module proxy build context the recipe declares to
	// the prefetched directory supplied under that name.
	Proxies map[string]string
}

// resolvePrivateModuleBuild reads GOPRIVATE from the host, taken as-is.
func resolvePrivateModuleBuild(lookupEnv func(string) (string, bool)) privateModuleBuild {
	var build privateModuleBuild
	if value, ok := lookupEnv(goPrivateBuildArg); ok {
		build.GoPrivate = strings.TrimSpace(value)
	}
	return build
}

// withProxies returns the build with the recipe's prefetched module proxies.
func (p privateModuleBuild) withProxies(proxies map[string]string) privateModuleBuild {
	p.Proxies = proxies
	return p
}

// buildxArgs renders the private-module inputs as docker buildx flags. A recipe
// that declares GOPRIVATE itself keeps its own value — a durable declaration
// beats the environment of one machine. Each prefetched proxy is a named build
// context, replacing the stage of that name the Dockerfile declares.
func (p privateModuleBuild) buildxArgs(recipeBuildArgs map[string]string) []string {
	var args []string
	if _, declared := recipeBuildArgs[goPrivateBuildArg]; p.GoPrivate != "" && !declared {
		args = append(args, "--build-arg", goPrivateBuildArg+"="+p.GoPrivate)
	}
	names := make([]string, 0, len(p.Proxies))
	for name := range p.Proxies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		args = append(args, "--build-context", name+"="+p.Proxies[name])
	}
	return args
}
