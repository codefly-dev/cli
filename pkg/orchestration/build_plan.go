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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/modulecache"

	"google.golang.org/protobuf/proto"

	"github.com/codefly-dev/cli/pkg/environments"
	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
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
func (b *Builder) buildFromPlan(ctx context.Context, outputDir string, plan *builderv0.DockerBuildPlan, prefetched map[string]map[string]string, reuse map[string]imageReuse) error {
	w := wool.Get(ctx).In("Builder.buildFromPlan", wool.ThisField(b.instance))
	if err := b.world.checkDisposableImagePolicy(); err != nil {
		return err
	}
	if err := coreservices.VerifyDockerBuildPlan(outputDir, plan); err != nil {
		return w.Wrapf(err, "cannot verify build recipe for %s", b.instance.Unique())
	}
	if err := verifyBuildImages(outputDir, plan, b.imageBindings); err != nil {
		return err
	}
	if err := b.checkDisposableImagePlan(plan); err != nil {
		return err
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
		// A decision the plan phase took stands; Build does not take it again.
		var decided *imageReuse
		if decision, ok := reuse[recipe.GetName()]; ok {
			decided = &decision
		}
		if err := b.buildRecipe(ctx, w, outputDir, contextRoot, plan, recipe, shouldPush, prefetched[recipe.GetName()], decided); err != nil {
			return err
		}
	}
	return nil
}

// imageReuse is the reuse decision for one recipe: the identity its inputs
// digest to and, when an image was already built from exactly those inputs and
// still exists, that image. A nil entry means the recipe is built.
type imageReuse struct {
	identity imageIdentity
	entry    *imageCacheEntry
	// image, when set, is the reference the recipe is pushed under instead of
	// the one the agent named: a build-qualified tag for a build whose agent the
	// workspace overrides (override_build.go).
	image string
}

// decideImageReuse takes the reuse decision for a recipe as it will be built
// (recipeAsBuilt), without provisioning a builder or fetching anything. A Go
// module proxy the recipe declares reaches the identity only as the
// placeholder every fetched proxy path is normalized to, so the decision does
// not wait for the fetch: a render whose images are all up to date fetches no
// module graph at all, where it used to download every Go service's graph into
// a fresh cache before finding out that no image input had changed.
func (b *Builder) decideImageReuse(
	ctx context.Context,
	w *wool.Wool,
	outputDir, contextRoot string,
	plan *builderv0.DockerBuildPlan,
	recipe *builderv0.DockerBuildRecipe,
	shouldPush bool,
) (imageReuse, error) {
	contextDir, err := recipeContext(contextRoot, recipe)
	if err != nil {
		return imageReuse{}, w.Wrapf(err, "cannot resolve build context for recipe %s of %s", recipe.GetName(), b.instance.Unique())
	}
	prepared, err := prepareRecipeContext(ctx, contextDir, outputDir, recipe)
	if err != nil {
		return imageReuse{}, w.Wrapf(err, "cannot prepare recipe context")
	}
	defer prepared.Close()

	multiArch := shouldPush && len(recipe.GetPlatforms()) > 1
	cache := scopedBuildCache(b.world.BuildCache, b.world.Workspace.Name, b.instance.Unique(), recipe.GetName())
	private := resolvePrivateModuleBuild(os.LookupEnv).withProxies(placeholderProxies(recipe))

	// Registry reads the reuse decision makes run on the caller's builder when it
	// provided one, because a registry reachable only from that builder is
	// reachable nowhere else; the CLI's own builder shares the host's registry
	// configuration, so it needs neither naming nor provisioning for a read.
	probeBuilder := b.world.BuildxBuilder
	// Where this environment reads a mirrored registry from. It reaches the
	// base-image resolution through the reference each manifest is read from
	// and is not part of the identity (see registry_mirror.go).
	mirrors := registryMirrors(b.world.Env)
	scope := imageBuildScope{OutputDir: outputDir, ContextDir: prepared.Root, WorkspaceDir: b.world.Workspace.Dir()}

	// The identity is derived from the invocation as it would run on any builder
	// and without a metadata file, so the decision to reuse is taken before a
	// builder is provisioned — provisioning one is work a reused image does not
	// need. normalizedBuildxArgs drops those same two flags from whatever argv it
	// is handed, so the two constructions cannot disagree about anything else.
	identityArgs, err := cachedBuildxArgs(recipe, prepared.Dockerfile, prepared.Root, shouldPush, multiArch, "", "", cache, private)
	if err != nil {
		return imageReuse{}, err
	}
	identity, identityErr := imageBuildIdentity(ctx, recipe, plan, scope, prepared.Dockerfile, probeBuilder, identityArgs, private.Proxies, mirrors, b.imageBindings...)
	if err = b.checkDisposableImageIdentity(identity, identityErr); err != nil {
		return imageReuse{}, err
	}
	if identityErr != nil {
		// Not a build failure: an input this cannot account for means the image
		// is rebuilt, which is the behaviour without a cache at all. It is said
		// out loud because the alternative is a workspace that silently rebuilds
		// everything forever and nobody knowing which input is unbindable.
		w.Warn("an image input cannot be bound; building, and this recipe will not be reused",
			wool.Field("recipe", recipe.GetName()), wool.ErrField(identityErr))
	}
	decision := imageReuse{identity: identity}
	// Scoped naming precedes identity; ordinary runtime naming derives from it.
	recipe, decision.image, err = b.imageForExecution(recipe, identity, identityErr)
	if err != nil {
		return decision, err
	}
	// A build under a workspace agent override is not the build the release
	// names, so it is pushed under a tag of its own (override_build.go).
	if shouldPush && releasedVersionTag(recipe.GetImage()) {
		pinned, overrideErr := b.overriddenAgent(ctx)
		if overrideErr != nil {
			return decision, w.Wrap(overrideErr)
		}
		if pinned != "" {
			if identity.Key == "" {
				return decision, w.NewError("%s builds with agent %s, which the workspace overrides from the %s its release pins, and an image input cannot be bound to name that build apart from the release: %v",
					b.instance.Unique(), b.instance.Service.Agent.Identifier(), pinned, identityErr)
			}
			recipe = proto.CloneOf(recipe)
			recipe.Image = overrideBuildImage(recipe.GetImage(), identity.Key)
			decision.image = recipe.GetImage()
			w.Info("the agent is overridden, so this is not the release's build; tagging it apart",
				wool.Field("agent", b.instance.Service.Agent.Identifier()), wool.Field("release_pins", pinned), wool.Field("image", decision.image))
		}
	}
	recipe, decision, err = qualifyBoundImage(recipe, decision, identityErr, shouldPush, b.imageBindings)
	if err != nil {
		return decision, err
	}
	if b.world.RebuildImages {
		return decision, nil
	}
	// A released version is built once. The image is tagged with the service's
	// version, which `codefly publish` stamps on every service of the release, and
	// a registry with immutable tags keeps that tag on that one build forever: so
	// when the registry already holds this service at this version, it is that
	// image, whichever environment, machine or checkout pushed it, and nothing is
	// built. An unversioned service (0.0.0, never published) is not a release and
	// is built as before.
	if shouldPush && (releasedVersionTag(recipe.GetImage()) || decision.image != "") {
		digest, err := resolveImageManifestDigest(ctx, recipe.GetImage(), probeBuilder)
		if err != nil && !isImageNotFound(err) {
			// Only the registry saying the tag does not exist means it is not
			// published: building over a probe that failed for any other reason
			// would overwrite a released tag wherever tags are mutable.
			return decision, w.Wrapf(err, "cannot tell whether %s is already published", recipe.GetImage())
		}
		if err == nil && digest != "" {
			// The version alone is not proof: a service whose version was not moved
			// with its code would hand back another release's image. The published
			// image records the inputs it was built from: the same inputs adopt it,
			// and other inputs adopt it only when its code is this build's code
			// (publishedReleaseVerdict) — what else differs is the agent's or the
			// registry's, and the release's build is the one already published.
			adopt, err := b.judgePublishedRelease(ctx, w, recipe, decision, digest, probeBuilder)
			if err != nil {
				return decision, err
			}
			if adopt != nil {
				decision.entry = adopt
				return decision, nil
			}
		}
	}
	if identity.Key == "" {
		return decision, nil
	}
	entry, found := b.imageBuildCache().lookup(identity.Key, b.instance.Unique(), shouldPush)
	switch {
	case found && b.cacheImageReferenceMismatch(entry, recipe):
		// The local tag is the claim about the inputs; an entry recorded under
		// another name cannot stand in for it.
		w.Debug("the recorded image is not named by these inputs; building",
			wool.Field("recipe", recipe.GetName()), wool.Field("recorded", entry.Image), wool.Field("identity", identity.Key))
	case found && imageStillExists(ctx, entry, probeBuilder):
		decision.entry = entry
	case found:
		w.Debug("an image was built from these inputs but no longer exists; building",
			wool.Field("recipe", recipe.GetName()), wool.Field("identity", identity.Key))
	default:
		w.Debug("no image has been built from these inputs; building",
			wool.Field("recipe", recipe.GetName()), wool.Field("identity", identity.Key))
	}
	return decision, nil
}

// judgePublishedRelease reads the build labels of a released tag the registry
// holds at digest and returns the entry that adopts it, nothing when it is to
// be built again (it records no inputs), or the refusal.
func (b *Builder) judgePublishedRelease(ctx context.Context, w *wool.Wool, recipe *builderv0.DockerBuildRecipe, decision imageReuse, digest, probeBuilder string) (*imageCacheEntry, error) {
	identity := decision.identity
	labels, _ := resolveImageBuildLabels(ctx, recipe.GetImage(), probeBuilder)
	published := labels[buildInputsLabel]
	adopted := &imageCacheEntry{
		Schema: imageCacheSchema, Key: identity.Key, Service: b.instance.Unique(), Recipe: recipe.GetName(),
		Image: recipe.GetImage(), Pushed: true, Digest: digest, Platforms: recipe.GetPlatforms(),
	}
	switch {
	case identity.Key != "" && published == identity.Key:
		return adopted, nil
	case identity.Key != "" && published != "" && decision.image != "":
		// A build-qualified tag names one identity; it is never adopted from
		// other inputs.
		return nil, w.Wrap(publishedFromOtherInputs(recipe.GetImage(), b.instance.Service.Agent, labels[buildAgentLabel]))
	case identity.Key != "" && published != "":
		pinned, err := modulecache.Owned(b.world.Workspace.Dir(), b.instance.Service.Dir())
		if err != nil {
			return nil, w.Wrapf(err, "cannot tell whether %s builds a pinned release", b.instance.Unique())
		}
		warning, refusal := publishedReleaseVerdict(recipe.GetImage(), identity, b.instance.Service.Agent, labels, pinned)
		if refusal != nil {
			return nil, w.Wrap(refusal)
		}
		w.Warn(warning, wool.Field("image", recipe.GetImage()), wool.Field("recipe", recipe.GetName()),
			wool.Field("published_inputs", published), wool.Field("identity", identity.Key))
		return adopted, nil
	default:
		w.Warn("the published image records no build inputs; building it again",
			wool.Field("image", recipe.GetImage()), wool.Field("recipe", recipe.GetName()))
		return nil, nil
	}
}

// placeholderProxies stands in for the Go module proxies and git source mirrors
// a recipe declares before they are fetched. normalizedBuildxArgs rewrites every fetched proxy
// path to this same placeholder, so an identity computed over these equals the
// one computed over the fetched paths — which is what lets the reuse decision
// precede the fetch.
func placeholderProxies(recipe *builderv0.DockerBuildRecipe) map[string]string {
	downloads := recipe.GetGoModuleDownloads()
	if len(downloads) == 0 && len(recipe.GetGitSourceDownloads()) == 0 {
		return nil
	}
	proxies := make(map[string]string, len(downloads)+len(recipe.GetGitSourceDownloads()))
	for _, download := range downloads {
		proxies[download.GetProxyContext()] = proxyPlaceholder
	}
	for _, download := range recipe.GetGitSourceDownloads() {
		proxies[download.GetMirrorContext()] = proxyPlaceholder
	}
	return proxies
}

// recipeAsBuilt is the recipe as buildRecipe builds it: targeted to the
// environment's platforms when the image is pushed, and named by the
// reference it is pushed to (pushReference) when the cell pulls it through a
// mirror. The reuse decision is taken on this form, so it binds the platforms
// the build will actually run and probes the registry the image is pushed to;
// the manifests keep the name the cell pulls, with the pushed digest.
func (b *Builder) recipeAsBuilt(recipe *builderv0.DockerBuildRecipe, shouldPush bool) (*builderv0.DockerBuildRecipe, error) {
	if err := b.world.checkDisposableImagePolicy(); err != nil {
		return nil, err
	}
	if b.world.disposableImageScope != "" {
		if shouldPush {
			return nil, fmt.Errorf("disposable images do not support push")
		}
		if built := b.executionRecipes[recipe.GetName()]; built != nil {
			return built, nil
		}
		if b.localRuntime {
			var err error
			recipe, err = b.localRuntimeRecipe(recipe)
			if err != nil {
				return nil, err
			}
		}
		return b.scopedRecipe(recipe), nil
	}
	if !shouldPush {
		if b.localRuntime {
			return b.localRuntimeRecipe(recipe)
		}
		return recipe, nil
	}
	targeted, err := b.targetEnvironmentPlatforms(recipe)
	if err != nil {
		return nil, err
	}
	if pushed := pushReference(b.world.Env, targeted.GetImage()); pushed != targeted.GetImage() {
		targeted = proto.CloneOf(targeted)
		targeted.Image = pushed
	}
	return targeted, nil
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
	prefetched map[string]string,
	reuse *imageReuse,
) error {
	recipe, err := b.recipeAsBuilt(recipe, shouldPush)
	if err != nil {
		return w.Wrapf(err, "cannot build recipe %s of %s", recipe.GetName(), b.instance.Unique())
	}

	// Image evidence is owed by every recipe, not only the one a snapshot pins,
	// so asking for it needs the digest of each pushed image too.
	resolveEvidence := b.world.CollectImageSBOM

	// The reuse decision comes before any build work, and one the plan phase
	// already took is not taken again.
	if reuse == nil {
		decided, decideErr := b.decideImageReuse(ctx, w, outputDir, contextRoot, plan, recipe, shouldPush)
		if decideErr != nil {
			return decideErr
		}
		reuse = &decided
	}
	if err = b.checkDisposableImageDecision(recipe, reuse); err != nil {
		return err
	}
	if reuse.image != "" {
		recipe = proto.CloneOf(recipe)
		recipe.Image = reuse.image
	}
	if b.localRuntime {
		b.localRuntimeBuilt = recipe
	}
	// A pushed build records the immutable manifest digest a snapshot pins and a
	// targeted single-service build returns to its caller. A non-pushed build
	// never lands in a registry, so there is no digest to capture. A build pushed
	// under a qualified tag is deployed by its digest whatever the mode: the
	// agent names the deployed image by the release's tag, which holds another
	// build.
	captureDigest := shouldPush && (b.world.Mode == SnapshotMode || b.world.CaptureImageDigest || reuse.image != "")
	if reuse.entry != nil {
		w.Info("no image input changed; keeping the built image",
			wool.Field("image", reuse.entry.Image), wool.Field("digest", reuse.entry.Digest),
			wool.Field("recipe", recipe.GetName()), wool.Field("identity", reuse.identity.Key))
		b.adoptCachedImage(recipe, reuse.entry, captureDigest, resolveEvidence)
		return nil
	}
	identity := reuse.identity
	// A recipe declaring Go module downloads is only built with the proxies
	// its plan phase fetched; building it without them would fall back to
	// fetching inside the build, which is what declaring them removes.
	if recipeMissesGoModuleProxies(recipe, prefetched) {
		return w.NewError("recipe %s of %s declares Go module downloads that were not fetched before the build", recipe.GetName(), b.instance.Unique())
	}
	// The same for a recipe declaring git sources and their mirrors.
	if recipeMissesGitSourceMirrors(recipe, prefetched) {
		return w.NewError("recipe %s of %s declares git sources that were not fetched before the build", recipe.GetName(), b.instance.Unique())
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

	cache := scopedBuildCache(b.world.BuildCache, b.world.Workspace.Name, b.instance.Unique(), recipe.GetName())
	// A service that imports a private Go module reads it from the proxy the
	// plan phase fetched; GOPRIVATE, a list of module paths and not a
	// credential, still reaches recipes that declare it as an ARG.
	private := resolvePrivateModuleBuild(os.LookupEnv).withProxies(prefetched)

	// Where this environment reads a mirrored registry from. It reaches the
	// build through the builder's buildkitd configuration and the base-image
	// resolution through the reference each manifest is read from; it is in
	// neither case part of the identity (see registry_mirror.go).
	mirrors := registryMirrors(b.world.Env)
	scope := imageBuildScope{OutputDir: outputDir, ContextDir: contextDir, WorkspaceDir: b.world.Workspace.Dir()}
	imageCache := b.imageBuildCache()

	builderName, err := b.selectImageBuilder(ctx, multiArch, mirrors)
	if err != nil {
		return w.Wrapf(err, "cannot provision image builder for %s", b.instance.Unique())
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
	if err == nil {
		args = withImageBindingsLabel(withBuildAgentLabel(withBuildInputsLabel(args, identity), b.instance.Service.Agent), b.imageBindings)
		if shouldPush {
			args = withSourceLabel(args, b.world.Env, workspaceSourceRepository(ctx, b.world.Workspace.Dir()), multiArch)
		}
	}
	if err != nil {
		return err
	}
	started := time.Now()
	w.Info("building image", wool.Field("image", recipe.GetImage()), wool.Field("push", shouldPush),
		wool.Field("goprivate", private.GoPrivate), wool.Field("prefetched_contexts", len(private.Proxies)))
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

// selectImageBuilder decides which buildx builder this recipe's build runs on,
// provisioning the CLI's own when the build needs one and the caller named
// none. An empty name leaves the build on the host's default builder.
//
// A caller-provided builder (e.g. a native amd64 buildkit) is authoritative: it
// owns whatever platforms the recipe declares, so the CLI neither provisions
// nor selects the local emulating builder. A cached build is already on the
// CLI's builder, provisioned by dockerBuildContext before the agent's Build
// RPC, so it is named rather than provisioned again here.
//
// Beyond a multi-platform build, which the default "docker" driver cannot
// produce at all, a declared registry mirror also requires the CLI's own
// builder whatever the platforms: the default builder takes no buildkitd
// configuration, so a single-platform build left on it would read its base from
// the registry the recipe names and the mirror would reach the resolution
// alone.
func (b *Builder) selectImageBuilder(ctx context.Context, multiArch bool, mirrors map[string]string) (string, error) {
	if name := b.world.BuildxBuilder; name != "" {
		if len(mirrors) > 0 {
			// The one place the mirror is declared and not applied to the pull:
			// a caller's builder carries its own buildkitd configuration. The
			// resolution still reads the mirror, so the digest is the same, but
			// the quota this exists to spare is spent anyway — said out loud
			// rather than left to be rediscovered as a 429.
			wool.Get(ctx).In("orchestration.selectImageBuilder").Warn(
				"this environment mirrors a registry, but the build runs on a supplied builder whose registry configuration the CLI does not set",
				wool.Field("builder", name), wool.Field("mirrors", mirrorSummary(mirrors)))
		}
		return name, nil
	}
	if b.world.BuildCache != nil {
		if b.world.disposableImageScope != "" {
			return ensureBuildxBuilder(ctx, mirrors)
		}
		return buildxBuilderFor(mirrors), nil
	}
	if !multiArch && len(mirrors) == 0 {
		return "", nil
	}
	return ensureBuildxBuilder(ctx, mirrors)
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
// used for multi-platform builds and for every build of an environment that
// declares a registry mirror, and returns the name it provisioned. It is
// idempotent and tolerates a concurrent creation racing another service's
// build. A builder created before host networking was required is reported as
// stale rather than reused: reusing it silently reproduces the ~90-minute
// tailnet DNS stall, and recreating it in place would race a sibling service
// mid-build on the shared builder, so the caller is told to remove it out of
// band.
//
// The mirrors decide both the builder's buildkitd registry configuration and,
// through it, its name (buildxBuilderFor), so a builder is never reused with a
// registry configuration other than the one asked for here.
func ensureBuildxBuilder(ctx context.Context, mirrors map[string]string) (string, error) {
	name := buildxBuilderFor(mirrors)
	w := wool.Get(ctx).In("orchestration.ensureBuildxBuilder")
	if len(mirrors) > 0 {
		// An environment that mirrors a registry builds on a builder of its
		// own, named for the configuration it carries. Which builder that is,
		// and what it redirects, is otherwise invisible in the render output.
		w.Info("this environment mirrors a registry; its builds run on their own buildx builder",
			wool.Field("builder", name), wool.Field("mirrors", mirrorSummary(mirrors)))
	}
	switch buildxBuilderState(ctx, name) {
	case buildxBuilderReady:
		return name, nil
	case buildxBuilderStale:
		return "", fmt.Errorf(
			"buildx builder %q predates host networking and cannot reach a tailnet split-DNS registry; run `docker buildx rm %s` and retry",
			name, name,
		)
	}
	config, removeConfig, err := stageBuildkitConfig(mirrors)
	if err != nil {
		return "", fmt.Errorf("stage the registry configuration of buildx builder %q: %w", name, err)
	}
	defer removeConfig()
	args := buildxCreateArgs(name, config)
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		if buildxBuilderState(ctx, name) == buildxBuilderReady {
			return name, nil
		}
		return "", fmt.Errorf("create buildx builder %q: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	w.Info("provisioned buildx builder", wool.Field("builder", name), wool.Field("mirrors", mirrorSummary(mirrors)))
	return name, nil
}

// buildxCreateArgs renders the docker buildx create argv for the dedicated
// container-driver builder. network=host runs BuildKit in the host network
// namespace so it inherits the host resolver and routes; without it the
// container-driver's isolated bridge net has its own resolver and cannot reach a
// tailnet split-DNS, so a push to a private-endpoint registry stalls on DNS
// resolution until its auth token ages out. It also grants the network.host
// build entitlement, which is acceptable for a builder running the recipes the
// CLI itself emits.
//
// A non-empty config is the buildkitd configuration the builder is created
// with, which is where a declared registry mirror reaches the build itself:
// BuildKit reads a base image through the mirror without the recipe naming it.
func buildxCreateArgs(name, config string) []string {
	args := []string{
		buildxCommand, "create",
		"--name", name,
		"--driver", "docker-container",
		"--driver-opt", "network=host",
	}
	if config != "" {
		args = append(args, "--config", config)
	}
	return append(args, "--bootstrap")
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
// not the daemon is running, so it does not depend on bootstrapping the node. A
// builder's buildkitd configuration is not rendered there at all, which is why
// the registry configuration is carried by the name instead (buildxBuilderFor).
func buildxBuilderState(ctx context.Context, name string) buildxBuilderStatus {
	output, err := exec.CommandContext(ctx, "docker", buildxCommand, "inspect", name).CombinedOutput()
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
// root (see buildRecipeRoot). The caller creates it before asking (Plan): the
// agent owns what is written there, and an agent that assembles its build
// context stats the directory before it writes.
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

// releasedVersionTag reports whether image is tagged with a released service
// version: an exact semantic version other than the unversioned 0.0.0 every
// service carries until `codefly publish` stamps its release on it. A
// pre-release, a moving name (latest) or a digest reference is not one.
func releasedVersionTag(image string) bool {
	if image == "" || strings.Contains(image, "@") {
		return false
	}
	last := image
	if separator := strings.LastIndex(image, "/"); separator >= 0 {
		last = image[separator+1:]
	}
	colon := strings.LastIndex(last, ":")
	if colon < 0 {
		return false
	}
	tag := strings.TrimPrefix(last[colon+1:], "v")
	return releasedVersion.MatchString(tag) && tag != "0.0.0"
}

var releasedVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// buildInputsLabel is the image label a build records its input identity under,
// so a later render can tell whether a published version holds exactly these
// inputs (decideImageReuse).
const buildInputsLabel = "org.codefly.build.inputs"

// The per-class labels a build records beside the aggregate (buildClasses), so
// a published release can be compared by what moved: its code, or only what
// the agent and the registry contribute.
const (
	buildCodeLabel   = buildInputsLabel + ".code"
	buildRecipeLabel = buildInputsLabel + ".recipe"
	buildBasesLabel  = buildInputsLabel + ".bases"
)

// buildLabels is every label resolveImageBuildLabels reads back.
var buildLabels = []string{buildInputsLabel, buildCodeLabel, buildRecipeLabel, buildBasesLabel, buildAgentLabel}

// withBuildInputsLabel records the build's input identity on the image: the
// aggregate and each class. It is added to the build's own argv after the
// identity was computed from it, so no label is an input to what it records.
func withBuildInputsLabel(args []string, identity imageIdentity) []string {
	if identity.Key == "" {
		return args
	}
	flags := []string{"--label", buildInputsLabel + "=" + identity.Key}
	if classes := identity.Classes; classes != nil {
		for _, class := range [][2]string{{buildCodeLabel, classes.Code}, {buildRecipeLabel, classes.Recipe}, {buildBasesLabel, classes.Bases}} {
			if class[1] != "" {
				flags = append(flags, "--label", class[0]+"="+class[1])
			}
		}
	}
	return withBuildFlags(args, flags...)
}

// publishedReleaseVerdict decides a released tag whose image records other
// inputs than this build's. The refusal exists to catch a version that did not
// move with its code, so only a code difference refuses; the published image is
// otherwise the release's build — rebuilding it would overwrite a release, and
// refusing would block every render of every cell holding it — and is adopted
// with a warning naming what differs. An agent recorded as other than the one
// the release pins still refuses: that tag holds a build made under an agent
// override (override_build.go), which the release does not name.
//
// An image labelled before the classes were recorded carries only the
// aggregate, so its code cannot be compared with this build's. It is adopted
// only when pinned says this build's code is a pinned release the CLI
// materialized (modulecache.Owned) — the release's own code, by construction —
// and refused otherwise: from a checkout, an edited service whose version did
// not move would be handed the earlier release's image with nothing compared.
func publishedReleaseVerdict(image string, identity imageIdentity, agent *resources.Agent, labels map[string]string, pinned bool) (string, error) {
	const release = "release a new version (codefly publish) instead of rebuilding a published one"
	publishedAgent := labels[buildAgentLabel]
	if agent != nil && publishedAgent != "" && publishedAgent != agent.Identifier() {
		return "", publishedFromOtherInputs(image, agent, publishedAgent)
	}
	classes := identity.Classes
	if classes == nil {
		classes = &buildClasses{}
	}
	code := labels[buildCodeLabel]
	switch {
	case code == "" && pinned:
		return "the published image predates per-class build labels; this build's code is the pinned release the CLI materialized, " +
			"so adopting it as the release's build (--rebuild builds it again)", nil
	case code == "":
		return "", fmt.Errorf("%s is already published, and it predates per-class build labels, so its code cannot be compared with this build's, "+
			"which is not a pinned release the CLI materialized (a checkout or an override): build from the pinned release, or %s", image, release)
	case code != classes.Code:
		return "", fmt.Errorf("%s is already published from other code: the service's build context or Go module manifests differ from the published build's, "+
			"so its code changed without its version moving; %s", image, release)
	}
	var differs []string
	if labels[buildRecipeLabel] != classes.Recipe {
		differs = append(differs, "the build recipe its agent emitted or the build invocation")
	}
	if labels[buildBasesLabel] != classes.Bases {
		differs = append(differs, "the resolved base images")
	}
	if len(differs) == 0 {
		differs = append(differs, "an input no class records")
	}
	return "the published image was built from this build's code, and " + strings.Join(differs, " and ") +
		" differ; adopting it as the release's build", nil
}

// withBuildFlags inserts flags right after the `build` subcommand of a buildx
// argv, leaving an argv with none unchanged.
func withBuildFlags(args []string, flags ...string) []string {
	for index, argument := range args {
		if argument == "build" {
			labelled := append([]string(nil), args[:index+1]...)
			labelled = append(labelled, flags...)
			return append(labelled, args[index+1:]...)
		}
	}
	return args
}

// imageSourceLabel is the OCI annotation GitHub Container Registry links a
// package to its repository by, which is how a private package inherits the
// repository's access.
const imageSourceLabel = "org.opencontainers.image.source"

// withSourceLabel links an image pushed to ghcr.io to the workspace's GitHub
// repository. Like withBuildInputsLabel it is added after the identity was
// computed, so it is never an input to it; a multi-platform push also
// annotates the index, which is the manifest the registry reads it from.
func withSourceLabel(args []string, env *environments.Environment, source string, multiArch bool) []string {
	if source == "" || env == nil || env.Registry == nil {
		return args
	}
	if host, _, _ := strings.Cut(env.Registry.URL, "/"); host != environments.GHCRRegistry {
		return args
	}
	flags := []string{"--label", imageSourceLabel + "=" + source}
	if multiArch {
		flags = append(flags, "--annotation", "index:"+imageSourceLabel+"="+source)
	}
	return withBuildFlags(args, flags...)
}

// githubRemote matches the GitHub remotes a workspace is cloned from.
var githubRemote = regexp.MustCompile(`^(?:git@github\.com:|(?:https|ssh)://(?:git@)?github\.com/)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// workspaceSourceRepository is the https URL of the GitHub repository the
// workspace is checked out from, or "" when it is not one. A seam for tests.
var workspaceSourceRepository = func(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	match := githubRemote.FindStringSubmatch(strings.TrimSpace(string(out)))
	if match == nil {
		return ""
	}
	return "https://github.com/" + match[1] + "/" + match[2]
}

// resolveImageBuildLabels reads the build labels every platform of a published
// image agrees on (buildLabels); a label a platform lacks
// or disagrees on is absent. A seam so a test needs no registry.
var resolveImageBuildLabels = func(ctx context.Context, reference, builderName string) (map[string]string, error) {
	probe, cancel := context.WithTimeout(ctx, imageRegistryProbeTimeout)
	defer cancel()
	args := []string{buildxCommand, "imagetools", "inspect"}
	if builderName != "" {
		args = append(args, "--builder", builderName)
	}
	args = append(args, "--format", "{{json .Image}}", reference)
	out, err := exec.CommandContext(probe, "docker", args...).Output()
	if err != nil {
		return nil, err
	}
	return buildLabelsFromImageConfig(out), nil
}

// buildLabelsFromImageConfig reads the build labels out of `imagetools
// inspect`'s image configuration: one configuration for a single-platform image,
// one per platform otherwise. A label is kept only when every platform records
// the same value.
func buildLabelsFromImageConfig(raw []byte) map[string]string {
	type config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	labels := map[string]string{}
	var single config
	if err := json.Unmarshal(raw, &single); err == nil && single.Config.Labels != nil {
		for _, key := range buildLabels {
			if value := single.Config.Labels[key]; value != "" {
				labels[key] = value
			}
		}
		return labels
	}
	var platforms map[string]config
	if err := json.Unmarshal(raw, &platforms); err != nil || len(platforms) == 0 {
		return labels
	}
	for _, key := range buildLabels {
		value := ""
		for _, platform := range platforms {
			label := platform.Config.Labels[key]
			if label == "" || (value != "" && label != value) {
				value = ""
				break
			}
			value = label
		}
		if value != "" {
			labels[key] = value
		}
	}
	return labels
}
