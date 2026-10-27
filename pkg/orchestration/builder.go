package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/environments"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	coreservices "github.com/codefly-dev/core/agents/services"
	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	"github.com/codefly-dev/core/services"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/wool"
)

/*
Builder is a wrapper around a builder service instance to fit the outputProperty interface
*/
type Builder struct {
	instance *services.Instance

	// API
	endpoints []*basev0.Endpoint

	world *World

	outputPropertyForLoad  *BuilderLoadManager
	outputPropertyForInit  *BuilderInitManager
	outputPropertyForBuild *BuilderBuildManager
	outputPropertyForSync  *BuilderSyncManager

	imageDigest string

	// localRuntime builds a recipe for this machine's container engine, keeping
	// generated files out of authored services and naming the image by its inputs.
	localRuntime bool
	// localRuntimeBuilt is the recipe a local runtime build ran, as built: named
	// by its inputs and narrowed to the engine's platform (recipeAsBuilt).
	localRuntimeBuilt *builderv0.DockerBuildRecipe

	imageEvidence  []*builderv0.ImageSBOM
	resolvedImages []coresbom.ResolvedImage

	// planned is the recipe the agent emitted in the plan phase, with its Go
	// module downloads already fetched, waiting for the build phase to execute
	// it. See Plan.
	planned *plannedBuild

	syncResponse     *builderv0.SyncResponse
	syncSkipped      bool
	deploymentOutput *builderv0.DeploymentOutput
	// deployedConfiguration is the configuration the service's Deploy exposed
	// to its consumers, as the builder returned it.
	deployedConfiguration *basev0.Configuration
	// deployedSecretKeys are the secret keys the service's own promotable
	// deployment reads from secret-<service> — the exact key set its own
	// ExternalSecret fetches from the store under its own scope.
	deployedSecretKeys []string
}

func NewBuilder(ctx context.Context, instance *services.Instance, world *World) (*Builder, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(instance.Identity))
	w.Debug("new builder")
	b := &Builder{
		instance: instance,

		world: world,

		outputPropertyForLoad:  NewBuilderLoadManager(instance.Unique()),
		outputPropertyForInit:  NewBuilderInitManager(instance.Unique()),
		outputPropertyForBuild: NewBuilderBuildManager(instance.Unique()),
		outputPropertyForSync:  NewBuilderSyncManager(instance.Unique()),
	}
	return b, nil
}

func (b *Builder) DeploymentOutput() *builderv0.DeploymentOutput {
	return b.deploymentOutput
}

// DeployedConfiguration returns the configuration the service's Deploy exposed
// to its consumers, or nil before a Deploy.
func (b *Builder) DeployedConfiguration() *basev0.Configuration {
	return b.deployedConfiguration
}

// DeployedSecretKeys returns the secret keys the service's own promotable
// deployment reads from secret-<service>, sorted, or nil before a promotable
// Deploy. They are the keys its own ExternalSecret fetches under its own
// scope, which is what a consumer assembling one of this service's templated
// values must be able to read.
func (b *Builder) DeployedSecretKeys() []string {
	return b.deployedSecretKeys
}

// ImageDigest returns the immutable registry manifest digest of the image this
// builder pushed, or "" when it built without pushing.
func (b *Builder) ImageDigest() string {
	return b.imageDigest
}

// ImageEvidence returns the digest-bound image SBOMs this builder collected, or
// nil when the flow did not ask for them.
func (b *Builder) ImageEvidence() []*builderv0.ImageSBOM {
	return b.imageEvidence
}

func (b *Builder) Load(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(b.instance))

	b.instance.Builder.Workspace = b.world.Workspace

	var options []services.BuilderLoadOption

	if b.world.Mode == SyncMode {
		options = []services.BuilderLoadOption{services.ForSync}
	}
	resp, err := b.instance.Builder.Load(ctx, options...)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder instance")
	}

	w.Debug("loaded", wool.Field("endpoints", resources.MakeManyEndpointSummary(resp.Endpoints)))

	b.endpoints = resp.Endpoints

	err = b.outputPropertyForLoad.Set(ctx, &BuilderLoadOutput{Endpoints: resp.Endpoints})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for load")
	}

	outputProperty, err := b.outputPropertyForLoad.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for load")
	}

	err = b.world.SharedState.RecordEndpoints(ctx, b.instance.Identity, resp.Endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record endpoints")
	}

	return outputProperty, nil
}

func (b *Builder) Init(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance))
	w.Debug("Init")

	dependenciesEndpoints, err := b.world.SharedState.GetDependenciesEndpoints(ctx, b.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get depednencies")
	}

	resp, err := b.instance.Builder.Init(ctx, &builderv0.InitRequest{
		DependenciesEndpoints: dependenciesEndpoints,
	})
	if err != nil {
		if grpcErr, ok := status.FromError(err); ok {
			// Now grpcErr is the unwrapped gRPC error
			// You can get the error code and message like this
			code := grpcErr.Code()
			message := grpcErr.Message()
			w.Debug("grpc", wool.Field("code", code), wool.Field("message", message))

			// Check if the error is a context cancelled error
			if code == codes.Canceled {
				return nil, nil
			}
		}
		return nil, w.Wrapf(err, "cannot call init")
	}

	if resp.State != nil && resp.State.State != builderv0.InitStatus_SUCCESS {
		return nil, w.NewError("service instance is not ready")
	}

	err = b.outputPropertyForInit.Set(ctx, &BuilderInitOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for init")
	}

	outputProperty, err := b.outputPropertyForInit.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for init")
	}

	return outputProperty, nil
}

func (b *Builder) Sync(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance))

	request := b.world.SyncRequest
	if request == nil {
		request = &builderv0.SyncRequest{}
	}
	if request.GetDryRun() {
		advertised, supported := ValidationOperationSupport(b.instance.Info, ValidationSync)
		if advertised && !supported {
			return nil, w.NewError("cannot prove sync drift for %s: agent explicitly does not support non-mutating sync", b.instance.Unique())
		}
		if !advertised {
			b.syncSkipped = true
			w.Debug("agent does not advertise a sync capability; skipping sync-drift")
			return nil, nil
		}
	}
	resp, err := b.instance.Builder.Sync(ctx, request, b.world.AnswerProvider)
	b.syncResponse = resp
	if err != nil {
		return nil, w.Wrapf(err, "cannot sync service instance")
	}
	if resp == nil || resp.State == nil {
		return nil, w.NewError("cannot sync %s: agent returned no status", b.instance.Unique())
	}
	if resp.State.State != builderv0.SyncStatus_SUCCESS {
		message := statusDiagnostic(resp.State.Message, "agent reported sync failure")
		return nil, w.NewError("cannot sync %s: %s", b.instance.Unique(), message)
	}

	err = b.outputPropertyForSync.Set(ctx, &BuilderSyncOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for sync")
	}

	outputProperty, err := b.outputPropertyForSync.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for sync")
	}

	return outputProperty, nil
}

func (b *Builder) SyncResponse() *builderv0.SyncResponse {
	return b.syncResponse
}

// SyncSkipped reports whether the last dry-run Sync was skipped because the
// agent advertises no authoritative sync capability contract.
func (b *Builder) SyncSkipped() bool {
	return b.syncSkipped
}

func (b *Builder) dockerBuildContext(ctx context.Context) (*builderv0.DockerBuildContext, error) {
	dockerContext, err := builder.DockerBuildContext(ctx, b.world.Workspace)
	if err != nil {
		return nil, err
	}
	if _, err = dockerhelpers.CacheArguments(b.world.BuildCache, coreservices.RecipeBuildPlatforms()); err != nil {
		return nil, fmt.Errorf("cannot configure build cache: %w", err)
	}
	dockerContext.Cache = scopedBuildCache(b.world.BuildCache, b.world.Workspace.Name, b.instance.Unique(), "")
	dockerContext.BuildxBuilder = b.world.BuildxBuilder
	mirrors := registryMirrors(b.world.Env)
	if dockerContext.BuildxBuilder == "" && (dockerContext.Cache != nil || len(mirrors) > 0) {
		if dockerContext.BuildxBuilder, err = ensureBuildxBuilder(ctx, mirrors); err != nil {
			return nil, fmt.Errorf("cannot provision buildx builder for %s: %w", b.instance.Unique(), err)
		}
	}
	return dockerContext, nil
}

// buildRecipeRoot resolves where this service's build recipe is emitted and
// archived (see the package-level buildRecipeRoot).
func (b *Builder) buildRecipeRoot() (string, error) {
	workspaceDir := ""
	if b.world != nil && b.world.Workspace != nil {
		workspaceDir = b.world.Workspace.Dir()
	}
	module, service := "", ""
	if b.instance.Identity != nil {
		module, service = b.instance.Identity.Module, b.instance.Identity.Name
	}
	if b.localRuntime {
		return localRuntimeRecipeRoot(workspaceDir, module, service)
	}
	return buildRecipeRoot(workspaceDir, module, service, b.instance.Service.Dir())
}

// plannedBuild is one service's emitted build: where its recipe lives, the
// agent's answer, and the Go module proxies and git source mirrors prefetched
// for each recipe.
type plannedBuild struct {
	recipeRoot string
	outputDir  string
	response   *builderv0.BuildResponse
	// prefetched maps a recipe name to its prefetched Go module proxies and git
	// source mirrors, keyed by the build-context name the recipe declares.
	prefetched map[string]map[string]string
	// reuse maps a recipe name to the reuse decision Plan took for it, for the
	// recipes it decided before fetching; Build adopts or builds from it.
	reuse map[string]imageReuse
}

// Plan asks the agent for the service's build recipe and fetches the Go module
// graphs the recipe declares, without building an image. A snapshot runs Plan
// for every service before it builds any (SnapshotPolicy), so every module is
// fetched at the start of the render, with the credentials the host has then,
// and no image build needs one. A recipe whose image was already built from
// exactly its current inputs fetches nothing: the reuse decision is taken here,
// before the fetch, and Build adopts that image. Build executes the planned
// recipe; a flow without a plan phase plans at the start of Build instead.
func (b *Builder) Plan(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance))
	w.Debug("Plan")
	if b.planned != nil {
		return OnInit(), nil
	}
	if advertised, supported := ValidationOperationSupport(b.instance.Info, ValidationArtifactBuild); advertised && !supported {
		return nil, w.NewError("cannot build deployable artifact for %s: agent explicitly advertises artifact build as unsupported", b.instance.Unique())
	}

	dockerContext, err := b.dockerBuildContext(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create build context")
	}

	recipeRoot, err := b.buildRecipeRoot()
	if err != nil {
		return nil, w.Wrapf(err, "cannot resolve build output root")
	}
	outputDir, err := buildRecipeOutputDirectory(recipeRoot)
	if err != nil {
		return nil, w.Wrapf(err, "cannot prepare build recipe directory")
	}
	// The directory the CLI asks the agent to emit into exists before the
	// agent is called: an agent that assembles a build context stats it first
	// (a uv path-source service copies its tree while skipping the recipe
	// directory by identity), and under the scratch root of a materialized
	// module nothing else ever created it.
	if err = os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, w.Wrapf(err, "cannot create build recipe directory %s", outputDir)
	}

	resp, err := b.instance.Builder.Build(ctx, &builderv0.BuildRequest{
		BuildContext:    builder.BuildContextFromDocker(dockerContext),
		OutputDirectory: outputDir,
	})
	if err != nil {
		w.Warn("cannot ask the agent for its image build plan", wool.Field("service", b.instance.Unique()), wool.ErrField(err))
		return nil, w.Wrapf(err, "cannot call build")
	}
	if resp == nil {
		return nil, w.Wrapf(fmt.Errorf("builder returned nil response without error"), "gRPC contract violation")
	}

	if resp.State != nil && resp.State.State != builderv0.BuildStatus_SUCCESS {
		return nil, w.NewError("call to build failed")
	}

	planned := &plannedBuild{recipeRoot: recipeRoot, outputDir: outputDir, response: resp}
	if plan := resp.Result.GetDockerBuildPlan(); plan != nil {
		// Fetch only for a recipe tree that verifies: a declared module root is
		// resolved against the context the verified plan selects.
		if err := coreservices.VerifyDockerBuildPlan(outputDir, plan); err != nil {
			return nil, w.Wrapf(err, "cannot verify build recipe for %s", b.instance.Unique())
		}
		contextRoot := recipeContextRoot(b.instance.Service.Dir(), outputDir, plan)
		for _, recipe := range plan.GetRecipes() {
			if len(recipe.GetGoModuleDownloads()) == 0 && len(recipe.GetGitSourceDownloads()) == 0 {
				continue
			}
			// Decide reuse before fetching, on the recipe as it will be built: an
			// image already built from exactly these inputs needs no module, so a
			// render whose images are up to date downloads nothing.
			built, err := b.recipeAsBuilt(recipe, b.world.Push)
			if err != nil {
				return nil, w.Wrapf(err, "cannot build recipe %s of %s", recipe.GetName(), b.instance.Unique())
			}
			reuse, err := b.decideImageReuse(ctx, w, outputDir, contextRoot, plan, built, b.world.Push)
			if err != nil {
				return nil, err
			}
			if planned.reuse == nil {
				planned.reuse = map[string]imageReuse{}
			}
			planned.reuse[recipe.GetName()] = reuse
			if reuse.entry != nil {
				w.Info("no image input changed; the Go modules and git sources this recipe declares are not fetched",
					wool.Field("recipe", recipe.GetName()), wool.Field("image", reuse.entry.Image))
				continue
			}
			contextDir, err := recipeContext(contextRoot, recipe)
			if err != nil {
				return nil, w.Wrapf(err, "cannot resolve build context for recipe %s of %s", recipe.GetName(), b.instance.Unique())
			}
			if len(recipe.GetGoModuleDownloads()) > 0 {
				w.Info("fetching Go modules before any image build", wool.Field("recipe", recipe.GetName()))
			}
			proxies, err := b.world.goModules().proxiesFor(ctx, contextDir, recipe)
			if err != nil {
				// What names the cause of a failed fetch is the go tool's own
				// output — a git error, a missing credential, an incomplete
				// go.sum — and the plain log renders an error chain collapsed to
				// its outermost messages, so that output only reached a --debug
				// re-run (codefly-dev/cli#886). Log it where it happened, so the
				// operator reads why the render stopped from the run that stopped.
				w.Warn("cannot fetch the Go modules this recipe declares",
					wool.Field("recipe", recipe.GetName()), wool.ErrField(err))
				return nil, w.Wrapf(err, "cannot fetch the Go modules of %s", b.instance.Unique())
			}
			// The same phase and route for a dependency locked to a git commit:
			// the host's git fetches it, the build reads a mirror context.
			if len(recipe.GetGitSourceDownloads()) > 0 {
				w.Info("fetching git sources before any image build", wool.Field("recipe", recipe.GetName()))
				mirrors, err := b.world.gitSources().mirrorsFor(ctx, recipe)
				if err != nil {
					w.Warn("cannot fetch the git sources this recipe declares",
						wool.Field("recipe", recipe.GetName()), wool.ErrField(err))
					return nil, w.Wrapf(err, "cannot fetch the git sources of %s", b.instance.Unique())
				}
				if proxies == nil {
					proxies = make(map[string]string, len(mirrors))
				}
				maps.Copy(proxies, mirrors)
			}
			if planned.prefetched == nil {
				planned.prefetched = map[string]map[string]string{}
			}
			planned.prefetched[recipe.GetName()] = proxies
		}
	}
	b.planned = planned
	return OnInit(), nil
}

// Build executes the service's planned recipe, planning it first when the flow
// has no plan phase.
func (b *Builder) Build(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance))
	w.Debug("Build")
	if b.planned == nil {
		if _, err := b.Plan(ctx); err != nil {
			return nil, err
		}
	}
	planned := b.planned
	b.planned = nil
	resp, outputDir, recipeRoot := planned.response, planned.outputDir, planned.recipeRoot

	// A build plan means the agent emitted recipes and the CLI owns the docker
	// build; otherwise the agent built in-process (legacy) and the CLI only pushes.
	plan := resp.Result.GetDockerBuildPlan()

	err := b.outputPropertyForBuild.Set(ctx, &BuilderBuildOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for build")
	}

	outputProperty, err := b.outputPropertyForBuild.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for build")
	}

	buildResult := dockerBuildResult(resp.Result)
	if plan != nil {
		if err = b.buildFromPlan(ctx, outputDir, plan, planned.prefetched, planned.reuse); err != nil {
			return nil, err
		}
	} else if buildResult != nil {
		if b.world.Push {
			w.Info("Pushing docker image", wool.Field("result", resp.Result))
			for _, im := range buildResult.Images {
				if err = verifyImageArchitecture(ctx, im); err != nil {
					return nil, w.Wrapf(err, "refusing to push %s", im)
				}
				if pushErr := pushLegacyImage(ctx, b.world.Env, im); pushErr != nil {
					return nil, w.Wrapf(pushErr, "cannot push docker image")
				}
			}
		}
		// The digest is read from the reference that was pushed: only it has a
		// repository digest.
		var deployed string
		if len(buildResult.Images) == 1 {
			deployed = buildResult.Images[0]
			if b.world.Push {
				deployed = pushReference(b.world.Env, deployed)
			}
		}
		if b.world.Mode == SnapshotMode && len(buildResult.Images) > 0 {
			if len(buildResult.Images) != 1 {
				return nil, w.NewError("snapshot build returned %d images for %s; exactly one deployable image is required", len(buildResult.Images), b.instance.Unique())
			}
			b.imageDigest, err = inspectImageDigest(ctx, deployed)
			if err != nil {
				return nil, w.Wrapf(err, "cannot resolve immutable image for %s", b.instance.Unique())
			}
		} else if b.world.CaptureImageDigest && b.world.Push && len(buildResult.Images) == 1 {
			// The image is already pushed; resolving its digest is best-effort
			// for reporting, so a lookup failure must not fail the build.
			if digest, digestErr := inspectImageDigest(ctx, deployed); digestErr != nil {
				w.Warn("built and pushed but could not resolve image digest", wool.ErrField(digestErr))
			} else {
				b.imageDigest = digest
			}
		}
	}

	// Record the durable recipe only after the build succeeded, so the committed
	// archive never advertises a recipe that failed verification or never built.
	if err = recordBuildRecipe(ctx, b.instance.Service, recipeRoot, plan); err != nil {
		return nil, w.Wrapf(err, "cannot record build recipe")
	}

	// Evidence is gathered after the recipe is durable: the image is already
	// built, and on a pushed build already published, so failing the evidence
	// gate must not also leave the recipe that produced it unrecorded.
	if b.world.CollectImageSBOM {
		if err = b.collectImageEvidence(ctx, plan, buildResult); err != nil {
			return nil, err
		}
	}

	return outputProperty, nil
}

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// runDocker runs one docker command of the legacy push path. A seam so a test
// needs no daemon.
var runDocker = func(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "docker", args...).Run()
}

// pushLegacyImage pushes an image an agent built in-process. One named by the
// cell's pull repository is tagged and pushed to the registry that repository
// mirrors (pushReference, recipeAsBuilt's rule).
func pushLegacyImage(ctx context.Context, env *environments.Environment, image string) error {
	pushed := pushReference(env, image)
	if pushed != image {
		if err := runDocker(ctx, "tag", image, pushed); err != nil {
			return fmt.Errorf("tag %s as %s: %w", image, pushed, err)
		}
	}
	return runDocker(ctx, "push", pushed)
}

func inspectImageDigest(ctx context.Context, image string) (string, error) {
	output, err := exec.CommandContext(
		ctx,
		"docker",
		"image",
		"inspect",
		"--format",
		"{{json .RepoDigests}}",
		image,
	).Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s repository digests: %w", image, err)
	}
	var references []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &references); err != nil {
		return "", fmt.Errorf("decode %s repository digests: %w", image, err)
	}
	var digest string
	for _, reference := range references {
		_, candidate, ok := strings.Cut(reference, "@")
		if !ok || !sha256Digest.MatchString(candidate) {
			continue
		}
		if digest != "" && digest != candidate {
			return "", fmt.Errorf("image %s resolves to multiple repository digests", image)
		}
		digest = candidate
	}
	if digest == "" {
		return "", fmt.Errorf("image %s has no registry-backed sha256 digest; build and push the local snapshot first", image)
	}
	return digest, nil
}

// deploymentImageArchitecture is the architecture deployment nodes run. A
// digest-pinned manifest names *an* image, not a compatible one, so an image
// built for the wrong architecture pushes and syncs cleanly and only fails at
// container exec (`exec format error`). Verifying before push turns that
// silent, far-downstream crash into a loud build failure.
const deploymentImageArchitecture = "amd64"

func verifyImageArchitecture(ctx context.Context, image string) error {
	output, err := exec.CommandContext(
		ctx,
		"docker",
		"image",
		"inspect",
		"--format",
		"{{.Architecture}}",
		image,
	).Output()
	if err != nil {
		return fmt.Errorf("inspect %s architecture: %w", image, err)
	}
	return checkImageArchitecture(image, strings.TrimSpace(string(output)))
}

func checkImageArchitecture(image, architecture string) error {
	if architecture == "" {
		return fmt.Errorf("image %s reports no architecture", image)
	}
	if architecture != deploymentImageArchitecture {
		return fmt.Errorf("image %s was built for %s but deployment nodes require %s; rebuild it targeting %s before pushing", image, architecture, deploymentImageArchitecture, deploymentImageArchitecture)
	}
	return nil
}

func dockerBuildResult(result *builderv0.BuildResult) *builderv0.DockerBuildResult {
	if result == nil {
		return nil
	}
	kind, ok := result.Kind.(*builderv0.BuildResult_DockerBuildResult)
	if !ok {
		return nil
	}
	return kind.DockerBuildResult
}

func (b *Builder) Unique() string {
	return b.instance.Unique()
}
