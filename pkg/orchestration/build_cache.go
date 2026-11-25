package orchestration

import (
	"encoding/json"
	"slices"

	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	"google.golang.org/protobuf/proto"
)

func scopedBuildCache(cache *builderv1.BuildCacheOptions, identity ...string) *builderv1.BuildCacheOptions {
	if cache == nil {
		return nil
	}
	result := proto.CloneOf(cache)
	if len(identity) > 0 {
		scope, _ := json.Marshal(append([]string{cache.Scope}, identity...))
		result.Scope = string(scope)
	}
	return result
}

func cachedBuildxArgs(recipe *builderv1.DockerBuildRecipe, dockerfile, contextDir string, push, multiArch bool, metadataFile, builderName string, cache *builderv1.BuildCacheOptions, private privateModuleBuild) ([]string, error) {
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
	args = slices.Concat(args[:len(args)-1], flags, private.buildxArgs(recipe.GetBuildArgs()), []string{"--progress", "plain", contextDir})
	return args, nil
}
