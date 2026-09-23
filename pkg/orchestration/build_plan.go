package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	// buildScratchDir is the workspace-relative root a service's build output
	// (builder/ and build-recipes/) is redirected to when the service tree is
	// not the workspace's own authored source: a CLI-materialized module (a
	// verified package under .codefly/cache, or a git clone under the module
	// cache root) is read-only from the CLI's point of view — its tree digest is
	// verified against the package, and a clone must stay byte-identical to the
	// commit it resolved. `.codefly/` is gitignored by core's composition.
	buildScratchDir = ".codefly/build"
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
	contextRoot := recipeContextRoot(b.instance.Service.Dir(), outputDir, plan)
	for _, recipe := range recipes {
		if err := b.buildRecipe(ctx, w, outputDir, contextRoot, recipe, shouldPush); err != nil {
			return err
		}
	}
	return nil
}

// recipeContextRoot is the directory a recipe's context is resolved against.
//
// The inventory scope says who assembled the context. A TREE plan claims every
// file under the recipe directory precisely because "the emitter assembled the
// whole destination — typically because it copied the build context there and a
// recipe builds \".\"" (RecipeInventoryScope in core's builder contract), and the
// CLI has just verified every one of those files against the plan's digests.
// Building the service directory instead silently discards whatever the emitter
// put there: a Go service whose module replaces a sibling by filesystem path
// emits a context carrying that sibling, and the build would resolve the
// original directive against a directory the image does not contain.
//
// An EMITTED plan claims only the files it wrote — the Dockerfile and its
// ignore — and the application's inputs are the service's own tree, so the
// context stays the service directory.
func recipeContextRoot(serviceDir, outputDir string, plan *builderv0.DockerBuildPlan) string {
	if plan.GetScope() == builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_TREE && assembledAContext(plan) {
		return outputDir
	}
	return serviceDir
}

// assembledAContext reports whether a TREE plan's inventory holds anything
// beyond the recipes' own build definitions.
//
// TRANSITION. A TREE claim says the emitter "assembled the whole destination",
// and this reads that claim against what the inventory actually lists: an
// inventory of nothing but Dockerfiles and ignore files states, truthfully,
// that no context was assembled, and the application's inputs are still the
// service's own tree. Agents published before the distinction was enforced
// declared TREE while emitting exactly that — service-go-grpc did until 0.1.46
// — and a consumer pins its agent version per service, so those emitters stay
// live long after the corrected agent ships. Without this, every one of them
// fails with `failed to compute cache key: "/code": not found`.
//
// Remove it once no released agent declares TREE for a build definition alone.
// It narrows nothing for a real TREE emitter: a plan that carried a context in
// lists those files too.
func assembledAContext(plan *builderv0.DockerBuildPlan) bool {
	definitions := make(map[string]struct{}, 2*len(plan.GetRecipes()))
	for _, recipe := range plan.GetRecipes() {
		definitions[recipe.GetDockerfile()] = struct{}{}
		if ignore := recipe.GetDockerignore(); ignore != "" {
			definitions[ignore] = struct{}{}
		}
	}
	for _, file := range plan.GetFiles() {
		if _, isDefinition := definitions[file.GetPath()]; !isDefinition {
			return true
		}
	}
	return false
}

// buildRecipe builds and (when pushing) publishes one recipe. It refuses to push
// an image that omits the deployment architecture, applies the recipe's declared
// ignore file, provisions a multi-platform builder when required, and resolves
// the pushed manifest digest for snapshot builds.
func (b *Builder) buildRecipe(
	ctx context.Context,
	w *wool.Wool,
	outputDir, contextRoot string,
	recipe *builderv0.DockerBuildRecipe,
	shouldPush bool,
) error {
	if shouldPush && !platformsIncludeDeploymentArch(recipe.GetPlatforms()) {
		return w.NewError(
			"recipe %s of %s targets platforms %v but deployment nodes require linux/%s; the recipe must build %s",
			recipe.GetName(), b.instance.Unique(), recipe.GetPlatforms(), deploymentImageArchitecture, deploymentImageArchitecture,
		)
	}
	contextDir, err := recipeContext(contextRoot, recipe)
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
	if multiArch && b.world.BuildCache == nil && builderName == "" {
		if err := ensureBuildxBuilder(ctx); err != nil {
			return w.Wrapf(err, "cannot provision image builder for %s", b.instance.Unique())
		}
		builderName = buildxBuilderName
	}

	if b.world.BuildCache != nil && builderName == "" {
		builderName = buildxBuilderName
	}

	// A pushed build records the immutable manifest digest a snapshot pins and a
	// targeted single-service build returns to its caller. A non-pushed build
	// never lands in a registry, so there is no digest to capture.
	captureDigest := shouldPush && (b.world.Mode == SnapshotMode || b.world.CaptureImageDigest)
	// Image evidence is owed by every recipe, not only the one a snapshot pins,
	// so asking for it needs the digest of each pushed image too.
	resolveEvidence := b.world.CollectImageSBOM
	var metadataFile string
	if captureDigest || (shouldPush && resolveEvidence) {
		file, err := os.CreateTemp("", "codefly-build-metadata-*.json")
		if err != nil {
			return w.Wrapf(err, "cannot stage build metadata for %s", b.instance.Unique())
		}
		metadataFile = file.Name()
		_ = file.Close()
		defer os.Remove(metadataFile)
	}

	cache := scopedBuildCache(b.world.BuildCache, b.world.Workspace.Name, b.instance.Unique(), recipe.GetName())
	// A service that imports a private Go module can only be built with the
	// host's GOPRIVATE and a credential; both come from the environment of the
	// machine building, resolved per build so a CI job's exported netrc is seen.
	private, err := resolvePrivateModuleBuild(os.LookupEnv, os.UserHomeDir)
	if err != nil {
		return w.Wrapf(err, "cannot resolve private module credentials for %s", b.instance.Unique())
	}
	args, err := cachedBuildxArgs(recipe, dockerfile, contextDir, shouldPush, multiArch, metadataFile, builderName, cache, private)
	if err != nil {
		return err
	}
	started := time.Now()
	// Log that a credential is mounted, never where it is or what it holds.
	w.Info("building image", wool.Field("image", recipe.GetImage()), wool.Field("push", shouldPush),
		wool.Field("goprivate", private.GoPrivate), wool.Field("netrc_mounted", private.Netrc != ""))
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return w.Wrapf(err, "cannot build %s", recipe.GetImage())
	}

	w.Info("image build completed", wool.Field("image", recipe.GetImage()), wool.Field("duration", time.Since(started)))

	if metadataFile != "" {
		digest, err := readPushedImageDigest(metadataFile)
		switch {
		case err != nil && b.world.Mode == SnapshotMode:
			// A snapshot's manifest pins this digest, so failing to resolve it
			// means the snapshot cannot be produced — a hard error.
			return w.Wrapf(err, "cannot resolve immutable image for %s", b.instance.Unique())
		case err != nil:
			// The image is already pushed; the digest is only reported. Failing
			// the build here would wrongly signal that the push failed. Evidence
			// derivation refuses the unpinned subject on its own.
			w.Warn("built and pushed but could not resolve image digest", wool.ErrField(err))
		default:
			if captureDigest {
				b.imageDigest = digest
			}
			b.recordPushedImage(recipe, digest)
		}
	}

	if resolveEvidence && !shouldPush {
		imageID, err := inspectLocalImageID(ctx, recipe.GetImage())
		if err != nil {
			return w.Wrapf(err, "cannot resolve the loaded image of recipe %s for %s", recipe.GetName(), b.instance.Unique())
		}
		b.recordLoadedImage(recipe, imageID)
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

// recipeContext resolves a recipe's build context against the root the plan's
// scope selects, and rejects a context that escapes it.
func recipeContext(root string, recipe *builderv0.DockerBuildRecipe) (string, error) {
	relative := recipe.GetContext()
	if relative == "" || relative == "." {
		return root, nil
	}
	contextDir := filepath.Join(root, filepath.FromSlash(relative))
	rel, err := filepath.Rel(root, contextDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recipe context %q escapes the build context root", relative)
	}
	return contextDir, nil
}

// preparedRecipeContext keeps source traversal and ignore matching with Docker.
// Only a custom build definition is staged, so concurrent recipes never mutate
// a shared Dockerfile.dockerignore or copy/filter the application's inputs.
type preparedRecipeContext struct {
	Root       string
	Dockerfile string
	directory  string
}

func (p *preparedRecipeContext) Close() error {
	if p.directory == "" {
		return nil
	}
	return os.RemoveAll(p.directory)
}

func prepareRecipeContext(ctx context.Context, contextDir, outputDir string, recipe *builderv0.DockerBuildRecipe) (*preparedRecipeContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(contextDir)
	if err != nil {
		return nil, err
	}
	dockerfile, err := recipeDockerfile(outputDir, recipe)
	if err != nil {
		return nil, err
	}
	if recipe.GetDockerignore() == "" {
		return &preparedRecipeContext{Root: root, Dockerfile: dockerfile}, nil
	}

	// Dockerfile-specific policy replaces root policy; it is not an additional
	// filter. Put the declared policy beside a private copy of the definition
	// and let Docker apply its native precedence and directory pruning.
	definition, err := os.ReadFile(dockerfile)
	if err != nil {
		return nil, err
	}
	ignore, err := os.ReadFile(filepath.Join(outputDir, recipe.GetDockerignore()))
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "codefly-build-definition-")
	if err != nil {
		return nil, err
	}
	definitionRoot, err := os.OpenRoot(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	defer definitionRoot.Close()
	if err := definitionRoot.WriteFile("Dockerfile", definition, 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	if err := definitionRoot.WriteFile("Dockerfile.dockerignore", ignore, 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	return &preparedRecipeContext{Root: root, Dockerfile: filepath.Join(directory, "Dockerfile"), directory: directory}, nil
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
// agent to emit recipes into: the builder/ directory under the service's build
// root (see buildRecipeRoot). It does not create the directory — the emitting
// agent owns writing there, so a legacy agent that ignores the field leaves no
// empty directory.
func buildRecipeOutputDirectory(root string) (string, error) {
	return filepath.Abs(filepath.Join(root, buildRecipeDir))
}

// buildRecipeRoot is the directory a service's build output — the builder/
// recipe the agent emits and the build-recipes/ archive — lands under.
//
// For a service authored in the workspace it is the service directory itself:
// builder/Dockerfile is committed there and the archive is versioned beside it.
// For a service the CLI materialized — anywhere outside the workspace
// directory, or under the workspace's own .codefly/ (where the verified
// package cache lives) — writing into the service tree would poison the
// package's tree digest and silently mutate a git-cloned module, so the output
// is redirected to <workspace>/.codefly/build/<module>/<service>. Only the
// recipe moves: the docker build context is still the service's code
// (recipeContext), and the recipe's Dockerfile and dockerignore resolve from
// this root (recipeDockerfile / prepareRecipeContext) — the same split the
// Build request already expresses as BuildContext vs OutputDirectory.
//
// An unset workspace directory cannot classify the service, so the output
// stays beside it.
func buildRecipeRoot(workspaceDir, module, service, serviceDir string) (string, error) {
	if workspaceDir == "" || serviceDir == "" {
		return serviceDir, nil
	}
	workspaceDir, err := filepath.Abs(workspaceDir)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(serviceDir)
	if err != nil {
		return "", err
	}
	if underDirectory(workspaceDir, absolute) && !underDirectory(filepath.Join(workspaceDir, ".codefly"), absolute) {
		return serviceDir, nil
	}
	if module == "" || service == "" {
		return "", fmt.Errorf("cannot redirect build output for %q: the service has no module/service identity", serviceDir)
	}
	return filepath.Join(workspaceDir, filepath.FromSlash(buildScratchDir), module, service), nil
}

// underDirectory reports whether path is dir or lies beneath it. Both must be
// absolute and clean.
func underDirectory(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
