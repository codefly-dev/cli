package common

import (
	"fmt"

	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

// BuildCacheFlags binds the two caches a build can use: the registry layer
// cache buildx imports and exports, and the workspace's own record of images it
// already built from the same inputs, which --rebuild opts out of.
type BuildCacheFlags struct {
	options builderv0.BuildCacheOptions
	rebuild bool
}

// defaultCacheBackend is the one transport the registry cache has; the flag
// default and Reset agree on it here.
const defaultCacheBackend = "registry"

// Rebuild reports whether the caller asked for every image to be built even
// when no input of it changed.
func (f *BuildCacheFlags) Rebuild() bool { return f.rebuild }

// Reset returns the flags to the values Bind gives them, in place. It is what
// a test that parsed arguments into package-level flags restores with: the
// options are a proto message, so snapshotting the struct to restore it later
// is the lock copy go vet refuses.
func (f *BuildCacheFlags) Reset() {
	f.rebuild = false
	proto.Reset(&f.options)
	f.options.Backend = defaultCacheBackend
}

func (f *BuildCacheFlags) Bind(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.BoolVar(&f.rebuild, "rebuild", false, "Build every image even when no input of it changed, instead of keeping the image already built from those inputs")
	flags.StringArrayVar(&f.options.Imports, "cache-from", nil, "Trusted registry cache repository to import (repeatable, without tag)")
	flags.StringArrayVar(&f.options.Exports, "cache-to", nil, "Registry cache repository to publish (repeatable; omit for read-only builds)")
	flags.StringVar(&f.options.Scope, "cache-scope", "", "Stable workspace and trust-domain cache scope; service, recipe and platform are added automatically")
	flags.StringVar(&f.options.Mode, "cache-mode", "", "Exported layers: max (default, includes dependencies) or min")
	flags.StringVar(&f.options.Backend, "cache-backend", defaultCacheBackend, "Cache transport backend (registry)")
}

func (f *BuildCacheFlags) Policy() (*builderv0.BuildCacheOptions, error) {
	if len(f.options.Imports) == 0 && len(f.options.Exports) == 0 && f.options.Scope == "" && f.options.Mode == "" && f.options.Backend == defaultCacheBackend {
		return nil, nil
	}
	if _, err := dockerhelpers.CacheArguments(&f.options, []string{"linux/amd64"}); err != nil {
		return nil, err
	}
	if len(f.options.Imports) == 0 && len(f.options.Exports) == 0 {
		return nil, fmt.Errorf("cache options require --cache-from or --cache-to")
	}
	return proto.CloneOf(&f.options), nil
}
