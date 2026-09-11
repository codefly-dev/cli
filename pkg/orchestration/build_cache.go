package orchestration

import (
	"encoding/json"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"google.golang.org/protobuf/proto"
)

func scopedBuildCache(cache *builderv0.BuildCacheOptions, identity ...string) *builderv0.BuildCacheOptions {
	if cache == nil {
		return nil
	}
	result := proto.Clone(cache).(*builderv0.BuildCacheOptions)
	if len(identity) > 0 {
		scope, _ := json.Marshal(append([]string{cache.Scope}, identity...))
		result.Scope = string(scope)
	}
	return result
}

func cachedBuildxArgs(recipe *builderv0.DockerBuildRecipe, dockerfile, contextDir string, push, multiArch bool, metadataFile, builderName string, cache *builderv0.BuildCacheOptions) ([]string, error) {
	platforms := recipe.GetPlatforms()
	if !push && len(platforms) > 1 {
		platforms = platforms[:1]
	}
	flags, err := dockerhelpers.CacheArguments(cache, platforms)
	if err != nil {
		return nil, err
	}
	args := buildxArgs(recipe, dockerfile, contextDir, push, multiArch, metadataFile, builderName)
	// The final positional argument must remain the prepared local context.
	args = append(args[:len(args)-1], append(flags, "--progress", "plain", contextDir)...)
	return args, nil
}
