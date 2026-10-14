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

	"google.golang.org/protobuf/proto"

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
	// buildxCommand is the docker subcommand every build and registry read goes
	// through.
	buildxCommand = "buildx"
)

// buildFromPlan owns the docker build the agent used to run in-process. It
// verifies the recipe tree the agent emitted, then runs docker buildx for each
// recipe — multi-arch and pushed as a manifest list when pushing — so the build
// recipe is a durable, first-class artifact and images are not tied to the
// builder's host architecture.
func (b *Builder) buildFromPlan(ctx context.Context, outputDir string, plan *builderv0.DockerBuildPlan, proxies map[string]map[string]string) error {
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
		// A recipe declaring Go module downloads is only built with the proxies
		// its plan phase fetched; building it without them would fall back to
		// fetching inside the build, which is what declaring them removes.
		if recipeMissesGoModuleProxies(recipe, proxies[recipe.GetName()]) {
			return w.NewError("recipe %s of %s declares Go module downloads that were not fetched before the build", recipe.GetName(), b.instance.Unique())
		}
		if err := b.buildRecipe(ctx, w, outputDir, contextRoot, plan, recipe, shouldPush, proxies[recipe.GetName()]); err != nil {
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
	if plan.GetScope() == builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_TREE && planAssembledAContext(plan) {
		return outputDir
	}
	return serviceDir
}

// planAssembledAContext reports whether a TREE plan's inventory holds anything
// beyond the recipes' own Dockerfiles and ignore files.
//
// Core states two things that only agree for an emitter that actually built a
// context. DockerBuildRecipe.context is "relative to the SERVICE directory —
// not to output_directory", which is every emitter's documented path; while
// RECIPE_INVENTORY_SCOPE_TREE describes an emitter that "assembled the whole
// destination — typically because it copied the build context there and a
// recipe builds '.'", whose context cannot be the service directory because the
// service directory is precisely what it does not contain.
//
// The inventory says which of the two a plan is, and it says so in the plan's
// own terms rather than by sniffing the filesystem: a plan that wrote only its
// Dockerfile and its ignore file assembled nothing, so its context is the
// service directory the field documents. This also keeps every agent that
// declared TREE before the distinction was enforced building exactly as it did
// — those plans write two files and nothing else — so the fleet does not have
// to be swept before this ships. Remove it once no released agent declares TREE
// without assembling, and the field and the scope can stop disagreeing.
func planAssembledAContext(plan *builderv0.DockerBuildPlan) bool {
	control := make(map[string]bool, 2*len(plan.GetRecipes()))
	for _, recipe := range plan.GetRecipes() {
		if path := recipe.GetDockerfile(); path != "" {
			control[filepath.ToSlash(filepath.Clean(path))] = true
		}
		if path := recipe.GetDockerignore(); path != "" {
			control[filepath.ToSlash(filepath.Clean(path))] = true
		}
	}
	for _, file := range plan.GetFiles() {
		if !control[filepath.ToSlash(filepath.Clean(file.GetPath()))] {
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
	plan *builderv0.DockerBuildPlan,
	recipe *builderv0.DockerBuildRecipe,
	shouldPush bool,
	goModuleProxies map[string]string,
) error {
	if err := b.prepareLocalRuntimeRecipe(recipe); err != nil {
		return err
	}
	if shouldPush {
		targeted, err := b.targetEnvironmentPlatforms(recipe)
		if err != nil {
			return w.Wrapf(err, "cannot build recipe %s of %s", recipe.GetName(), b.instance.Unique())
		}
		recipe = targeted
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

	multiArch := shouldPush && len(recipe.GetPlatforms()) > 1

	// A pushed build records the immutable manifest digest a snapshot pins and a
	// targeted single-service build returns to its caller. A non-pushed build
	// never lands in a registry, so there is no digest to capture.
	captureDigest := shouldPush && (b.world.Mode == SnapshotMode || b.world.CaptureImageDigest)
	// Image evidence is owed by every recipe, not only the one a snapshot pins,
	// so asking for it needs the digest of each pushed image too.
	resolveEvidence := b.world.CollectImageSBOM

	cache := scopedBuildCache(b.world.BuildCache, b.world.Workspace.Name, b.instance.Unique(), recipe.GetName())
	// A service that imports a private Go module reads it from the proxy the
	// plan phase fetched; GOPRIVATE, a list of module paths and not a
	// credential, still reaches recipes that declare it as an ARG.
	private := resolvePrivateModuleBuild(os.LookupEnv).withProxies(goModuleProxies)

	// Registry reads the reuse decision makes run on the caller's builder when it
	// provided one, because a registry reachable only from that builder is
	// reachable nowhere else; the CLI's own builder shares the host's registry
	// configuration, so it needs neither naming nor provisioning for a read.
	probeBuilder := b.world.BuildxBuilder
	scope := imageBuildScope{OutputDir: outputDir, ContextDir: contextDir, WorkspaceDir: b.world.Workspace.Dir()}

	// The identity is derived from the invocation as it would run on any builder
	// and without a metadata file, so the decision to reuse is taken before a
	// builder is provisioned — provisioning one is work a reused image does not
	// need. normalizedBuildxArgs drops those same two flags from whatever argv it
	// is handed, so the two constructions cannot disagree about anything else.
	identityArgs, err := cachedBuildxArgs(recipe, dockerfile, contextDir, shouldPush, multiArch, "", "", cache, private)
	if err != nil {
		return err
	}
	identity, identityErr := imageBuildIdentity(ctx, recipe, plan, scope, dockerfile, probeBuilder, identityArgs, private.Proxies)
	if err = b.nameLocalRuntimeImage(recipe, identity, identityErr); err != nil {
		return err
	}
	if identityErr != nil {
		// Not a build failure: an input this cannot account for means the image
		// is rebuilt, which is the behaviour without a cache at all. It is said
		// out loud because the alternative is a workspace that silently rebuilds
		// everything forever and nobody knowing which input is unbindable.
		w.Warn("an image input cannot be bound; building, and this recipe will not be reused",
			wool.Field("recipe", recipe.GetName()), wool.ErrField(identityErr))
	}
	imageCache := b.imageBuildCache()
	if b.reuseBuiltImage(ctx, w, recipe, identity, imageCache, captureDigest, resolveEvidence) {
		return nil
	}

	// A caller-provided builder (e.g. a native amd64 buildkit) is authoritative:
	// it owns whatever platforms the recipe declares, so the CLI neither
	// provisions nor selects the local emulating builder.
	builderName := b.world.BuildxBuilder
	if multiArch && b.world.BuildCache == nil && builderName == "" {
		if provisionErr := ensureBuildxBuilder(ctx); provisionErr != nil {
			return w.Wrapf(provisionErr, "cannot provision image builder for %s", b.instance.Unique())
		}
		builderName = buildxBuilderName
	}

	if b.world.BuildCache != nil && builderName == "" {
		builderName = buildxBuilderName
	}

	// A pushed build also resolves its digest when it has an entry to record:
	// the digest is the only identity a later run can verify the pushed image
	// against, so a build that does not resolve one cannot be reused.
	recordable := identity.Key != "" && imageCache.enabled()
	metadataFile, removeMetadata, err := b.stageBuildMetadata(captureDigest || (shouldPush && (resolveEvidence || recordable)))
	if err != nil {
		return w.Wrapf(err, "cannot stage build metadata for %s", b.instance.Unique())
	}
	defer removeMetadata()

	args, err := cachedBuildxArgs(recipe, dockerfile, contextDir, shouldPush, multiArch, metadataFile, builderName, cache, private)
	if err != nil {
		return err
	}
	started := time.Now()
	w.Info("building image", wool.Field("image", recipe.GetImage()), wool.Field("push", shouldPush),
		wool.Field("goprivate", private.GoPrivate), wool.Field("go_module_proxies", len(private.Proxies)))
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if buildErr := command.Run(); buildErr != nil {
		return w.Wrapf(buildErr, "cannot build %s", recipe.GetImage())
	}

	w.Info("image build completed", wool.Field("image", recipe.GetImage()), wool.Field("duration", time.Since(started)))

	pushedDigest, err := b.resolvePushedDigest(w, metadataFile, recipe, captureDigest)
	if err != nil {
		return err
	}
	loadedImageID, err := b.resolveLoadedImage(ctx, w, recipe, shouldPush, resolveEvidence, recordable)
	if err != nil {
		return err
	}
	if err = b.verifyLocalRuntimeBuild(ctx, w, recipe, scope, identity); err != nil {
		return err
	}
	b.recordImageForReuse(ctx, w, recipe, scope, identity, imageCache, shouldPush, pushedDigest, loadedImageID, recordable)
	return nil
}

func (b *Builder) reuseBuiltImage(ctx context.Context, w *wool.Wool, recipe *builderv0.DockerBuildRecipe,
	identity imageIdentity, cache *imageBuildCache, captureDigest, resolveEvidence bool,
) bool {
	if identity.Key == "" || b.world.RebuildImages {
		return false
	}
	entry, found := cache.lookup(identity.Key, b.instance.Unique(), b.world.Push)
	if !found {
		w.Debug("no image has been built from these inputs; building",
			wool.Field("recipe", recipe.GetName()), wool.Field("identity", identity.Key))
		return false
	}
	if (b.localRuntime && entry.Image != recipe.GetImage()) || !imageStillExists(ctx, entry, b.world.BuildxBuilder) {
		w.Debug("the recorded image cannot be reused; building",
			wool.Field("recipe", recipe.GetName()), wool.Field("identity", identity.Key))
		return false
	}
	w.Info("no image input changed; keeping the built image",
		wool.Field("image", entry.Image), wool.Field("digest", entry.Digest),
		wool.Field("recipe", recipe.GetName()), wool.Field("identity", identity.Key))
	b.adoptCachedImage(recipe, entry, captureDigest, resolveEvidence)
	return true
}

// resolvePushedDigest reads the immutable manifest digest of a pushed build from
// the metadata file, when one was staged.
//
// Failing to resolve it is fatal for a snapshot and reported for anything else:
// a snapshot's manifest pins this digest, so without it the snapshot cannot be
// produced, while elsewhere the image is already pushed and failing the build
// would wrongly signal that the push failed. Evidence derivation refuses the
// unpinned subject on its own.
func (b *Builder) resolvePushedDigest(
	w *wool.Wool, metadataFile string, recipe *builderv0.DockerBuildRecipe, captureDigest bool,
) (string, error) {
	if metadataFile == "" {
		return "", nil
	}
	digest, err := readPushedImageDigest(metadataFile)
	switch {
	case err != nil && b.world.Mode == SnapshotMode:
		return "", w.Wrapf(err, "cannot resolve immutable image for %s", b.instance.Unique())
	case err != nil:
		w.Warn("built and pushed but could not resolve image digest", wool.ErrField(err))
		return "", nil
	}
	if captureDigest {
		b.imageDigest = digest
	}
	b.recordPushedImage(recipe, digest)
	return digest, nil
}

// resolveLoadedImage identifies the image a non-pushed build loaded locally,
// which is the only identity a later run could stand in for.
//
// Fatal when evidence was asked for and merely noted otherwise: without an
// identity there is nothing to record, so no later run may reuse this build.
func (b *Builder) resolveLoadedImage(
	ctx context.Context, w *wool.Wool, recipe *builderv0.DockerBuildRecipe,
	shouldPush, resolveEvidence, recordable bool,
) (string, error) {
	if shouldPush || (!resolveEvidence && !recordable) {
		return "", nil
	}
	imageID, err := inspectLocalImageID(ctx, recipe.GetImage())
	switch {
	case err != nil && resolveEvidence:
		return "", w.Wrapf(err, "cannot resolve the loaded image of recipe %s for %s", recipe.GetName(), b.instance.Unique())
	case err != nil:
		w.Debug("built image cannot be identified; recording no reuse entry", wool.ErrField(err))
		return "", nil
	}
	if resolveEvidence {
		b.recordLoadedImage(recipe, imageID)
	}
	return imageID, nil
}

// recordImageForReuse writes the reuse entry for a build whose inputs held
// still. A failed write is a warning, never a build failure: the image is
// built, and the next run simply rebuilds, which is the behaviour without a
// cache at all.
func (b *Builder) recordImageForReuse(
	ctx context.Context, w *wool.Wool, recipe *builderv0.DockerBuildRecipe, scope imageBuildScope,
	identity imageIdentity, imageCache *imageBuildCache, shouldPush bool,
	pushedDigest, loadedImageID string, recordable bool,
) {
	if !recordable || !b.contextHeldStill(ctx, w, recipe, scope, identity) {
		return
	}
	entry := cachedImageEntry(identity.Key, b.instance.Unique(), recipe, shouldPush, pushedDigest, loadedImageID)
	if entry == nil {
		return
	}
	if err := imageCache.store(ctx, entry); err != nil {
		w.Warn("cannot record the image build for reuse", wool.ErrField(err))
	}
	w.Debug("recorded the image build for reuse", wool.Field("recipe", recipe.GetName()), wool.Field("identity", identity.Key))
}

// contextHeldStill reports whether the build context is still the tree the
// identity was taken over.
//
// The identity is computed before the build and the record is written after it,
// and buildx reads the context somewhere in between — so a file edited during a
// build that takes minutes produces an image the key does not describe. Recording
// it would leave an entry claiming the pre-edit inputs and serving the post-edit
// image, which is a false hit waiting for the tree to come back to where it was:
// a revert, a branch switch, a stash pop. Nothing is recorded in that case. The
// cost is one rebuild; the cost of the alternative is a stale image nobody can
// see.
func (b *Builder) contextHeldStill(ctx context.Context, w *wool.Wool, recipe *builderv0.DockerBuildRecipe, scope imageBuildScope, identity imageIdentity) bool {
	after, err := contextDigest(ctx, recipe, scope)
	if err != nil {
		w.Debug("cannot re-read the build context after the build; recording no reuse entry",
			wool.Field("recipe", recipe.GetName()), wool.ErrField(err))
		return false
	}
	if after == identity.ContextTree {
		return true
	}
	w.Warn("the build context changed while the image was being built; recording no reuse entry",
		wool.Field("recipe", recipe.GetName()), wool.Field("before", identity.ContextTree), wool.Field("after", after))
	return false
}

// adoptCachedImage makes a reused entry indistinguishable to the rest of the
// flow from a build that just ran: the digest a snapshot pins and the resolved
// image every SBOM subject is bound to come from the entry, which is only
// adopted after the image it names has been proven to still exist.
func (b *Builder) adoptCachedImage(recipe *builderv0.DockerBuildRecipe, entry *imageCacheEntry, captureDigest, resolveEvidence bool) {
	if entry.Pushed {
		if captureDigest {
			b.imageDigest = entry.Digest
		}
		b.recordPushedImage(recipe, entry.Digest)
		return
	}
	if resolveEvidence {
		b.recordLoadedImage(recipe, entry.ImageID)
	}
}

// buildxArgs renders the docker buildx argv for one recipe. A push builds every
// requested platform into one manifest list; a local build cannot materialize a
// multi-platform manifest list, so it targets a single platform and loads it
// into the daemon. A caller-provided builder wins; otherwise multi-platform
// builds run on the dedicated container-driver builder. A metadata file captures
// the pushed manifest digest.
func buildxArgs(recipe *builderv0.DockerBuildRecipe, dockerfile, contextDir string, push, multiArch bool, metadataFile, builderName string) []string {
	args := []string{buildxCommand, "build"}
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
		buildxCommand, "create",
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
	output, err := exec.CommandContext(ctx, "docker", buildxCommand, "inspect", buildxBuilderName).CombinedOutput()
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

// targetEnvironmentPlatforms narrows a pushed recipe to the platforms of the
// environment the flow builds for: the architectures its cluster's nodes run
// (Environment.ImagePlatforms). A recipe declares every platform it can build;
// the environment decides which of them its images need, so a render for an
// amd64 cell builds amd64 natively and never emulates an architecture no node
// runs. A platform the environment needs and the recipe cannot build is refused.
//
// A render or a deploy always targets an environment, so one that does not
// declare its architectures is refused (fail early). A plain pushed build of an
// environment that declares no cluster publishes a portable artifact rather
// than an image for a cell: it keeps the recipe's platforms, which must still
// include the architecture every deployment so far has run on.
func (b *Builder) targetEnvironmentPlatforms(recipe *builderv0.DockerBuildRecipe) (*builderv0.DockerBuildRecipe, error) {
	env := b.world.Env
	targetsCell := b.world.Mode == SnapshotMode || b.world.Mode == DeployMode
	if env == nil || ((env.Cluster == nil || env.Cluster.Kind == "") && !targetsCell) {
		if !platformsIncludeDeploymentArch(recipe.GetPlatforms()) {
			return nil, fmt.Errorf("recipe targets platforms %v but a portable image must include linux/%s; declare the target environment's cluster.architectures to build for it instead",
				recipe.GetPlatforms(), deploymentImageArchitecture)
		}
		return recipe, nil
	}
	platforms, err := b.world.imagePlatforms()
	if err != nil {
		return nil, err
	}
	for _, platform := range platforms {
		if !recipeBuildsPlatform(recipe.GetPlatforms(), platform) {
			return nil, fmt.Errorf("environment %q runs %s, which the recipe does not build (it builds %v)", env.Name, platform, recipe.GetPlatforms())
		}
	}
	narrowed := proto.CloneOf(recipe)
	narrowed.Platforms = platforms
	return narrowed, nil
}

// recipeBuildsPlatform reports whether a recipe's platforms include platform,
// comparing os/arch and ignoring a variant the recipe names ("linux/amd64/v2"
// builds linux/amd64).
func recipeBuildsPlatform(platforms []string, platform string) bool {
	for _, candidate := range platforms {
		fields := strings.Split(candidate, "/")
		if len(fields) >= 2 && fields[0]+"/"+fields[1] == platform {
			return true
		}
	}
	return false
}

// stageBuildMetadata stages the file buildx writes its result to, when this
// build has a reason to read one: a digest to capture, evidence to derive, or a
// reuse entry to record. It returns the path and the cleanup, which is a no-op
// when no file was wanted.
func (b *Builder) stageBuildMetadata(wanted bool) (string, func(), error) {
	if !wanted {
		return "", func() {}, nil
	}
	file, err := os.CreateTemp("", "codefly-build-metadata-*.json")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	_ = file.Close()
	return path, func() { _ = os.Remove(path) }, nil
}
