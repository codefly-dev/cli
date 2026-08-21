package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/wool"
)

// buildxBuilderName is the dedicated docker-container buildx builder the CLI
// creates for multi-platform builds. The default buildx builder uses the
// "docker" driver, which cannot build multiple platforms.
const buildxBuilderName = "codefly"

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
	recipes := plan.GetRecipes()
	if len(recipes) == 0 {
		return w.NewError("build plan for %s contains no recipes", b.instance.Unique())
	}
	shouldPush := push.Load()
	if b.world.Mode == SnapshotMode {
		if len(recipes) != 1 {
			return w.NewError("snapshot build for %s emitted %d recipes; exactly one deployable image is required", b.instance.Unique(), len(recipes))
		}
		if !shouldPush {
			return w.NewError("snapshot build for %s requires push to resolve an immutable image digest", b.instance.Unique())
		}
	}
	for _, recipe := range recipes {
		if err := b.buildRecipe(ctx, w, outputDir, recipe, shouldPush); err != nil {
			return err
		}
	}
	return nil
}

// buildRecipe builds and (when pushing) publishes one recipe. It refuses to push
// an image that omits the deployment architecture, applies the recipe's declared
// ignore file, provisions a multi-platform builder when required, and resolves
// the pushed manifest digest for snapshot builds.
func (b *Builder) buildRecipe(
	ctx context.Context,
	w *wool.Wool,
	outputDir string,
	recipe *builderv0.DockerBuildRecipe,
	shouldPush bool,
) error {
	if shouldPush && !platformsIncludeDeploymentArch(recipe.GetPlatforms()) {
		return w.NewError(
			"recipe %s of %s targets platforms %v but deployment nodes require linux/%s; the recipe must build %s",
			recipe.GetName(), b.instance.Unique(), recipe.GetPlatforms(), deploymentImageArchitecture, deploymentImageArchitecture,
		)
	}
	dockerfile, err := recipeDockerfile(outputDir, recipe)
	if err != nil {
		return w.Wrapf(err, "cannot resolve dockerfile for recipe %s of %s", recipe.GetName(), b.instance.Unique())
	}
	contextDir, err := recipeContext(outputDir, recipe)
	if err != nil {
		return w.Wrapf(err, "cannot resolve build context for recipe %s of %s", recipe.GetName(), b.instance.Unique())
	}

	cleanupIgnore, err := applyRecipeIgnore(outputDir, dockerfile, recipe)
	if err != nil {
		return w.Wrapf(err, "cannot apply ignore file for recipe %s of %s", recipe.GetName(), b.instance.Unique())
	}
	defer cleanupIgnore()

	multiArch := shouldPush && len(recipe.GetPlatforms()) > 1
	if multiArch {
		if err := ensureBuildxBuilder(ctx); err != nil {
			return w.Wrapf(err, "cannot provision multi-architecture builder for %s", b.instance.Unique())
		}
	}

	var metadataFile string
	if b.world.Mode == SnapshotMode {
		file, err := os.CreateTemp("", "codefly-build-metadata-*.json")
		if err != nil {
			return w.Wrapf(err, "cannot stage build metadata for %s", b.instance.Unique())
		}
		metadataFile = file.Name()
		_ = file.Close()
		defer os.Remove(metadataFile)
	}

	args := buildxArgs(recipe, dockerfile, contextDir, shouldPush, multiArch, metadataFile)
	w.Info("building image", wool.Field("image", recipe.GetImage()), wool.Field("push", shouldPush))
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return w.Wrapf(err, "cannot build %s", recipe.GetImage())
	}

	if b.world.Mode == SnapshotMode {
		digest, err := readPushedImageDigest(metadataFile)
		if err != nil {
			return w.Wrapf(err, "cannot resolve immutable image for %s", b.instance.Unique())
		}
		b.imageDigest = digest
	}
	return nil
}

// buildxArgs renders the docker buildx argv for one recipe. A push builds every
// requested platform into one manifest list; a local build cannot materialize a
// multi-platform manifest list, so it targets a single platform and loads it
// into the daemon. Multi-platform builds run on the dedicated container-driver
// builder, and a metadata file captures the pushed manifest digest.
func buildxArgs(recipe *builderv0.DockerBuildRecipe, dockerfile, contextDir string, push, multiArch bool, metadataFile string) []string {
	args := []string{"buildx", "build"}
	if multiArch {
		args = append(args, "--builder", buildxBuilderName)
	}
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
	if metadataFile != "" {
		args = append(args, "--metadata-file", metadataFile)
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

// platformsIncludeDeploymentArch reports whether the recipe builds the
// architecture deployment nodes run. An empty platform list also fails: without
// an explicit platform buildx builds only the builder's host architecture, which
// on Apple silicon is the arm64 image that cannot run on amd64 nodes.
func platformsIncludeDeploymentArch(platforms []string) bool {
	for _, platform := range platforms {
		fields := strings.Split(platform, "/")
		if len(fields) >= 2 && fields[1] == deploymentImageArchitecture {
			return true
		}
	}
	return false
}

// recipeDockerfile resolves a recipe's Dockerfile within the emitted recipe tree
// and rejects a path that escapes it. VerifyDockerBuildPlan digests only the file
// tree, not the recipe fields, so an unconstrained dockerfile could point
// buildx -f at an out-of-tree file — the same escape recipeContext guards for the
// build context.
func recipeDockerfile(outputDir string, recipe *builderv0.DockerBuildRecipe) (string, error) {
	dockerfile := filepath.Join(outputDir, filepath.FromSlash(recipe.GetDockerfile()))
	rel, err := filepath.Rel(outputDir, dockerfile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recipe dockerfile %q escapes the recipe directory", recipe.GetDockerfile())
	}
	return dockerfile, nil
}

// recipeContext resolves a recipe's build context and rejects a context that
// escapes the service directory.
func recipeContext(serviceDir string, recipe *builderv0.DockerBuildRecipe) (string, error) {
	relative := recipe.GetContext()
	if relative == "" || relative == "." {
		return serviceDir, nil
	}
	contextDir := filepath.Join(serviceDir, filepath.FromSlash(relative))
	rel, err := filepath.Rel(serviceDir, contextDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recipe context %q escapes the service directory", relative)
	}
	return contextDir, nil
}

// applyRecipeIgnore makes the recipe's declared ignore file visible to buildx,
// which only discovers "<dockerfile>.dockerignore" or "<context>/.dockerignore"
// — never the "builder/dockerignore" name agents emit. It writes the ignore to
// the discovered sibling path for the duration of the build and returns a
// cleanup. It is a no-op when the recipe declares no ignore or already emits it
// at the discovered path.
func applyRecipeIgnore(outputDir, dockerfile string, recipe *builderv0.DockerBuildRecipe) (func(), error) {
	ignore := recipe.GetDockerignore()
	if ignore == "" {
		return func() {}, nil
	}
	source := filepath.Join(outputDir, filepath.FromSlash(ignore))
	target := dockerfile + ".dockerignore"
	if source == target {
		return func() {}, nil
	}
	input, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("stage recipe ignore at %s: %w", target, err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		_ = os.Remove(target)
		return nil, err
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(target)
		return nil, err
	}
	return func() { _ = os.Remove(target) }, nil
}

// ensureBuildxBuilder provisions the dedicated docker-container buildx builder
// used for multi-platform builds. It is idempotent and tolerates a concurrent
// creation racing another service's build.
func ensureBuildxBuilder(ctx context.Context) error {
	if buildxBuilderExists(ctx) {
		return nil
	}
	output, err := exec.CommandContext(
		ctx, "docker", "buildx", "create",
		"--name", buildxBuilderName, "--driver", "docker-container", "--bootstrap",
	).CombinedOutput()
	if err != nil {
		if buildxBuilderExists(ctx) {
			return nil
		}
		return fmt.Errorf("create buildx builder %q: %w: %s", buildxBuilderName, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func buildxBuilderExists(ctx context.Context) bool {
	return exec.CommandContext(ctx, "docker", "buildx", "inspect", buildxBuilderName).Run() == nil
}

// readPushedImageDigest reads the registry manifest digest buildx recorded in
// its metadata file. A pushed multi-platform build never lands in the local
// image store, so the digest cannot be recovered with docker image inspect.
func readPushedImageDigest(metadataFile string) (string, error) {
	input, err := os.Open(metadataFile)
	if err != nil {
		return "", err
	}
	defer input.Close()
	data, err := io.ReadAll(input)
	if err != nil {
		return "", err
	}
	var metadata struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("decode build metadata: %w", err)
	}
	if !sha256Digest.MatchString(metadata.Digest) {
		return "", fmt.Errorf("build produced no registry-backed sha256 digest; push the image before pinning it")
	}
	return metadata.Digest, nil
}

// buildRecipeOutputDirectory is the absolute destination the caller asks the
// agent to emit recipes into: the service directory itself. It is the build
// context and the recipe tree root — recipes reference builder/Dockerfile and a
// "." context relative to it, and the digest inventory covers it. It does not
// create the directory (it already exists as the service dir).
func buildRecipeOutputDirectory(serviceDir string) (string, error) {
	return filepath.Abs(serviceDir)
}
