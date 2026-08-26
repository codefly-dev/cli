package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/builder"
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
	endpoints       []*basev0.Endpoint
	networkMappings []*basev0.NetworkMapping

	world *World

	// Requires
	requires []string

	// outputProperty hub
	isStarted bool

	outputPropertyForLoad  *BuilderLoadManager
	outputPropertyForInit  *BuilderInitManager
	outputPropertyForBuild *BuilderBuildManager
	outputPropertyForSync  *BuilderSyncManager

	push        bool
	imageDigest string

	syncResponse     *builderv0.SyncResponse
	syncSkipped      bool
	deploymentOutput *builderv0.DeploymentOutput
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

func (b *Builder) Load(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(b.instance))

	b.instance.Builder.Workspace = b.world.Workspace

	var options []services.BuilderLoadOption

	switch b.world.Mode {
	case SyncMode:
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

func (b *Builder) Build(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance))
	w.Debug("Build")
	if advertised, supported := ValidationOperationSupport(b.instance.Info, ValidationArtifactBuild); advertised && !supported {
		return nil, w.NewError("cannot build deployable artifact for %s: agent explicitly advertises artifact build as unsupported", b.instance.Unique())
	}

	// Build the request
	dockerContext, err := builder.DockerBuildContext(ctx, b.world.Workspace)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create build context")
	}

	outputDir, err := buildRecipeOutputDirectory(b.instance.Service.Dir())
	if err != nil {
		return nil, w.Wrapf(err, "cannot prepare build recipe directory")
	}

	resp, err := b.instance.Builder.Build(ctx, &builderv0.BuildRequest{
		BuildContext:    builder.BuildContextFromDocker(dockerContext),
		OutputDirectory: outputDir,
	})
	if err != nil {
		return nil, w.Wrapf(err, "cannot call build")
	}
	if resp == nil {
		return nil, w.Wrapf(fmt.Errorf("builder returned nil response without error"), "gRPC contract violation")
	}

	if resp.State != nil && resp.State.State != builderv0.BuildStatus_SUCCESS {
		return nil, w.NewError("call to build failed")
	}

	if err = recordBuildRecipe(ctx, b.instance.Service); err != nil {
		return nil, w.Wrapf(err, "cannot record build recipe")
	}

	err = b.outputPropertyForBuild.Set(ctx, &BuilderBuildOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for build")
	}

	outputProperty, err := b.outputPropertyForBuild.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for build")
	}

	// A build plan means the agent emitted recipes and the CLI owns the docker
	// build; otherwise the agent built in-process (legacy) and the CLI only pushes.
	if plan := resp.Result.GetDockerBuildPlan(); plan != nil {
		if err = b.buildFromPlan(ctx, outputDir, plan); err != nil {
			return nil, err
		}
	} else if buildResult := dockerBuildResult(resp.Result); buildResult != nil {
		if b.world.Push {
			w.Info("Pushing docker image", wool.Field("result", resp.Result))
			for _, im := range buildResult.Images {
				if err = verifyImageArchitecture(ctx, im); err != nil {
					return nil, w.Wrapf(err, "refusing to push %s", im)
				}
				cmd := exec.CommandContext(ctx, "docker", "push", im)
				err := cmd.Run()
				if err != nil {
					return nil, w.Wrapf(err, "cannot push docker image")
				}
			}
		}
		if b.world.Mode == SnapshotMode && len(buildResult.Images) > 0 {
			if len(buildResult.Images) != 1 {
				return nil, w.NewError("snapshot build returned %d images for %s; exactly one deployable image is required", len(buildResult.Images), b.instance.Unique())
			}
			b.imageDigest, err = inspectImageDigest(ctx, buildResult.Images[0])
			if err != nil {
				return nil, w.Wrapf(err, "cannot resolve immutable image for %s", b.instance.Unique())
			}
		}
	}
	return outputProperty, nil
}

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

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
