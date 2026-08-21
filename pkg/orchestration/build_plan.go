package orchestration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/wool"
)

// buildRecipeDir is the service-relative directory an agent emits its build
// recipes into. It is the committed, durable location a consumer rebuilds from
// (docker buildx -f services/<svc>/builder/Dockerfile services/<svc>), and the
// Dockerfiles COPY builder/… paths relative to the service-directory context.
const buildRecipeDir = "builder"

// buildFromPlan owns the docker build the agent used to run in-process. It
// verifies the recipe tree the agent emitted, then runs docker buildx for each
// recipe — multi-arch and pushed as a manifest list when pushing — so the build
// recipe is a durable, first-class artifact and images are not tied to the
// builder's host architecture.
func (b *Builder) buildFromPlan(ctx context.Context, outputDir string, plan *builderv0.DockerBuildPlan) error {
	w := wool.Get(ctx).In("Builder.buildFromPlan", wool.ThisField(b.instance))
	if err := coreservices.VerifyDockerBuildPlan(outputDir, plan); err != nil {
		return w.Wrapf(err, "cannot verify build recipe for %s", b.instance.Unique())
	}
	shouldPush := push.Load()
	if b.world.Mode == SnapshotMode {
		if len(plan.GetRecipes()) != 1 {
			return w.NewError("snapshot build for %s emitted %d recipes; exactly one deployable image is required", b.instance.Unique(), len(plan.GetRecipes()))
		}
		if !shouldPush {
			return w.NewError("snapshot build for %s requires push to resolve an immutable image digest", b.instance.Unique())
		}
	}
	serviceDir := b.instance.Service.Dir()
	for _, recipe := range plan.GetRecipes() {
		dockerfile := filepath.Join(outputDir, filepath.FromSlash(recipe.GetDockerfile()))
		contextDir := serviceDir
		if relative := recipe.GetContext(); relative != "" && relative != "." {
			contextDir = filepath.Join(serviceDir, filepath.FromSlash(relative))
		}
		args := buildxArgs(recipe, dockerfile, contextDir, shouldPush)
		w.Info("building image", wool.Field("image", recipe.GetImage()), wool.Field("push", shouldPush))
		if output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
			return w.Wrapf(err, "cannot build %s: %s", recipe.GetImage(), strings.TrimSpace(string(output)))
		}
		if b.world.Mode == SnapshotMode {
			digest, err := inspectImageDigest(ctx, recipe.GetImage())
			if err != nil {
				return w.Wrapf(err, "cannot resolve immutable image for %s", b.instance.Unique())
			}
			b.imageDigest = digest
		}
	}
	return nil
}

// buildxArgs renders the docker buildx argv for one recipe. A push builds every
// requested platform into one manifest list; a local build cannot materialize a
// multi-platform manifest list, so it targets a single platform and loads it
// into the daemon.
func buildxArgs(recipe *builderv0.DockerBuildRecipe, dockerfile, contextDir string, push bool) []string {
	args := []string{"buildx", "build"}
	platforms := recipe.GetPlatforms()
	if push {
		if len(platforms) > 0 {
			args = append(args, "--platform", strings.Join(platforms, ","))
		}
		args = append(args, "--push")
	} else {
		if len(platforms) > 0 {
			args = append(args, "--platform", platforms[0])
		}
		args = append(args, "--load")
	}
	if target := recipe.GetTarget(); target != "" {
		args = append(args, "--target", target)
	}
	buildArgs := recipe.GetBuildArgs()
	keys := make([]string, 0, len(buildArgs))
	for key := range buildArgs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", key, buildArgs[key]))
	}
	return append(args, "-t", recipe.GetImage(), "-f", dockerfile, contextDir)
}

// buildRecipeOutputDirectory is the absolute destination the caller asks the
// agent to emit recipes into: the committed builder/ directory under the service.
func buildRecipeOutputDirectory(serviceDir string) (string, error) {
	outputDir, err := filepath.Abs(filepath.Join(serviceDir, buildRecipeDir))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", err
	}
	return outputDir, nil
}
