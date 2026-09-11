package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/wool"
)

const (
	// buildRecipeDir is the service-relative directory an agent emits its build
	// recipes into. It is the committed, durable location a consumer rebuilds from
	// (docker buildx -f services/<svc>/builder/Dockerfile services/<svc>), and the
	// Dockerfiles COPY builder/… paths relative to the service-directory context.
	buildRecipeDir = "builder"
	// buildxBuilderName is the dedicated docker-container buildx builder the CLI
	// creates for multi-platform builds. The default buildx builder uses the
	// "docker" driver, which cannot build multiple platforms.
	buildxBuilderName = "codefly"
)

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
	shouldPush := b.world.Push
	if b.world.Mode == SnapshotMode {
		if len(recipes) != 1 {
			return w.NewError("snapshot build for %s emitted %d recipes; exactly one deployable image is required", b.instance.Unique(), len(recipes))
		}
		if !shouldPush {
			return w.NewError("snapshot build for %s requires push to resolve an immutable image digest", b.instance.Unique())
		}
	}
	serviceDir := b.instance.Service.Dir()
	for _, recipe := range recipes {
		if err := b.buildRecipe(ctx, w, outputDir, serviceDir, recipe, shouldPush); err != nil {
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
	outputDir, serviceDir string,
	recipe *builderv0.DockerBuildRecipe,
	shouldPush bool,
) error {
	if shouldPush && !platformsIncludeDeploymentArch(recipe.GetPlatforms()) {
		return w.NewError(
			"recipe %s of %s targets platforms %v but deployment nodes require linux/%s; the recipe must build %s",
			recipe.GetName(), b.instance.Unique(), recipe.GetPlatforms(), deploymentImageArchitecture, deploymentImageArchitecture,
		)
	}
	contextDir, err := recipeContext(serviceDir, recipe)
	if err != nil {
		return w.Wrapf(err, "cannot resolve build context for recipe %s of %s", recipe.GetName(), b.instance.Unique())
	}

	prepared, err := prepareRecipeContext(ctx, contextDir, outputDir, recipe)
	if err != nil {
		return w.Wrapf(err, "cannot prepare recipe context")
	}
	defer prepared.Close()
	dockerfile := prepared.Dockerfile
	contextDir = prepared.Root

	// A caller-provided builder (e.g. a native amd64 buildkit) is authoritative:
	// it owns whatever platforms the recipe declares, so the CLI neither
	// provisions nor selects the local emulating builder.
	builderName := b.world.BuildxBuilder
	multiArch := shouldPush && len(recipe.GetPlatforms()) > 1
	if (multiArch || b.world.BuildCache != nil) && builderName == "" {
		if err := ensureBuildxBuilder(ctx); err != nil {
			return w.Wrapf(err, "cannot provision image builder for %s", b.instance.Unique())
		}
		builderName = buildxBuilderName
	}

	// A pushed build records the immutable manifest digest a snapshot pins and a
	// targeted single-service build returns to its caller. A non-pushed build
	// never lands in a registry, so there is no digest to capture.
	captureDigest := shouldPush && (b.world.Mode == SnapshotMode || b.world.CaptureImageDigest)
	var metadataFile string
	if captureDigest {
		file, err := os.CreateTemp("", "codefly-build-metadata-*.json")
		if err != nil {
			return w.Wrapf(err, "cannot stage build metadata for %s", b.instance.Unique())
		}
		metadataFile = file.Name()
		_ = file.Close()
		defer os.Remove(metadataFile)
	}

	cache := scopedBuildCache(b.world.BuildCache, b.instance.Unique(), recipe.GetName())
	args, err := cachedBuildxArgs(recipe, dockerfile, contextDir, shouldPush, multiArch, metadataFile, builderName, cache)
	if err != nil {
		return err
	}
	started := time.Now()
	w.Info("building image", wool.Field("image", recipe.GetImage()), wool.Field("push", shouldPush))
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return w.Wrapf(err, "cannot build %s", recipe.GetImage())
	}

	w.Info("image build completed", wool.Field("image", recipe.GetImage()), wool.Field("duration", time.Since(started)))

	if captureDigest {
		digest, err := readPushedImageDigest(metadataFile)
		switch {
		case err != nil && b.world.Mode == SnapshotMode:
			// A snapshot's manifest pins this digest, so failing to resolve it
			// means the snapshot cannot be produced — a hard error.
			return w.Wrapf(err, "cannot resolve immutable image for %s", b.instance.Unique())
		case err != nil:
			// The image is already pushed; the digest is only reported. Failing
			// the build here would wrongly signal that the push failed.
			w.Warn("built and pushed but could not resolve image digest", wool.ErrField(err))
		default:
			b.imageDigest = digest
		}
	}
	return nil
}

// buildxArgs renders the docker buildx argv for one recipe. A push builds every
// requested platform into one manifest list; a local build cannot materialize a
// multi-platform manifest list, so it targets a single platform and loads it
// into the daemon. A caller-provided builder wins; otherwise multi-platform
// builds run on the dedicated container-driver builder. A metadata file captures
// the pushed manifest digest.
func buildxArgs(recipe *builderv0.DockerBuildRecipe, dockerfile, contextDir string, push, multiArch bool, metadataFile, builderName string) []string {
	args := []string{"buildx", "build"}
	switch {
	case builderName != "":
		args = append(args, "--builder", builderName)
	case multiArch:
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

func prepareRecipeContext(ctx context.Context, contextDir, outputDir string, recipe *builderv0.DockerBuildRecipe) (*dockerhelpers.PreparedBuildContext, error) {
	dockerfile, err := recipeDockerfile(outputDir, recipe)
	if err != nil {
		return nil, err
	}
	relativeDockerfile, err := filepath.Rel(contextDir, dockerfile)
	if err != nil {
		return nil, err
	}
	var ignore string
	if recipe.GetDockerignore() != "" {
		ignore, err = filepath.Rel(contextDir, filepath.Join(outputDir, recipe.GetDockerignore()))
		if err != nil {
			return nil, err
		}
	}
	return dockerhelpers.PrepareBuildContext(ctx, contextDir, relativeDockerfile, ignore)
}

// ensureBuildxBuilder provisions the dedicated docker-container buildx builder
// used for multi-platform builds. It is idempotent and tolerates a concurrent
// creation racing another service's build. A builder created before host
// networking was required is reported as stale rather than reused: reusing it
// silently reproduces the ~90-minute tailnet DNS stall, and recreating it in
// place would race a sibling service mid-build on the shared builder, so the
// caller is told to remove it out of band.
func ensureBuildxBuilder(ctx context.Context) error {
	switch buildxBuilderState(ctx) {
	case buildxBuilderReady:
		return nil
	case buildxBuilderStale:
		return fmt.Errorf(
			"buildx builder %q predates host networking and cannot reach a tailnet split-DNS registry; run `docker buildx rm %s` and retry",
			buildxBuilderName, buildxBuilderName,
		)
	}
	args := buildxCreateArgs()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		if buildxBuilderState(ctx) == buildxBuilderReady {
			return nil
		}
		return fmt.Errorf("create buildx builder %q: %w: %s", buildxBuilderName, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// buildxCreateArgs renders the docker buildx create argv for the dedicated
// container-driver builder. network=host runs BuildKit in the host network
// namespace so it inherits the host resolver and routes; without it the
// container-driver's isolated bridge net has its own resolver and cannot reach a
// tailnet split-DNS, so a push to a private-endpoint registry stalls on DNS
// resolution until its auth token ages out. It also grants the network.host
// build entitlement, which is acceptable for a builder running the recipes the
// CLI itself emits.
func buildxCreateArgs() []string {
	return []string{
		"buildx", "create",
		"--name", buildxBuilderName,
		"--driver", "docker-container",
		"--driver-opt", "network=host",
		"--bootstrap",
	}
}

type buildxBuilderStatus int

const (
	buildxBuilderMissing buildxBuilderStatus = iota
	buildxBuilderReady
	buildxBuilderStale
)

// buildxBuilderState reports whether the codefly builder is absent, present with
// host networking, or present without it (a pre-fix builder). Detection reads the
// builder's stored driver options, which docker buildx inspect renders whether or
// not the daemon is running, so it does not depend on bootstrapping the node.
func buildxBuilderState(ctx context.Context) buildxBuilderStatus {
	output, err := exec.CommandContext(ctx, "docker", "buildx", "inspect", buildxBuilderName).CombinedOutput()
	if err != nil {
		return buildxBuilderMissing
	}
	if builderHasHostNetwork(output) {
		return buildxBuilderReady
	}
	return buildxBuilderStale
}

func builderHasHostNetwork(inspectOutput []byte) bool {
	return bytes.Contains(inspectOutput, []byte(`network="host"`)) ||
		bytes.Contains(inspectOutput, []byte("network=host"))
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
// agent to emit recipes into: the committed builder/ directory under the
// service. It does not create the directory — the emitting agent owns writing
// there, so a legacy agent that ignores the field leaves no empty directory.
func buildRecipeOutputDirectory(serviceDir string) (string, error) {
	return filepath.Abs(filepath.Join(serviceDir, buildRecipeDir))
}
