package common

import (
	"fmt"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

type BuildCacheFlags struct{ options builderv0.BuildCacheOptions }

func (f *BuildCacheFlags) Bind(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringArrayVar(&f.options.Imports, "cache-from", nil, "Trusted registry cache repository to import (repeatable, without tag)")
	flags.StringArrayVar(&f.options.Exports, "cache-to", nil, "Registry cache repository to publish (repeatable; omit for read-only builds)")
	flags.StringVar(&f.options.Scope, "cache-scope", "", "Stable workspace and trust-domain cache scope; service, recipe and platform are added automatically")
	flags.StringVar(&f.options.Mode, "cache-mode", "", "Exported layers: max (default, includes dependencies) or min")
	flags.StringVar(&f.options.Backend, "cache-backend", "registry", "Cache transport backend (registry)")
}

func (f *BuildCacheFlags) Policy() (*builderv0.BuildCacheOptions, error) {
	if len(f.options.Imports) == 0 && len(f.options.Exports) == 0 && f.options.Scope == "" && f.options.Mode == "" && f.options.Backend == "registry" {
		return nil, nil
	}
	if _, err := dockerhelpers.CacheArguments(&f.options, []string{"linux/amd64"}); err != nil {
		return nil, err
	}
	if len(f.options.Imports) == 0 && len(f.options.Exports) == 0 {
		return nil, fmt.Errorf("cache options require --cache-from or --cache-to")
	}
	return proto.Clone(&f.options).(*builderv0.BuildCacheOptions), nil
}
