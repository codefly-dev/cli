package orchestration

import (
	"context"
	"fmt"
	"os/exec"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/cli/communicate"
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

	push bool
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

	// Build the request
	resp, err := b.instance.Builder.Sync(ctx, &builderv0.SyncRequest{}, communicate.NewPrompt())
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

func (b *Builder) Build(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance))
	w.Debug("Build")

	// Build the request
	dockerContext, err := builder.DockerBuildContext(ctx, b.world.Workspace)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create build context")
	}

	resp, err := b.instance.Builder.Build(ctx, &builderv0.BuildRequest{BuildContext: builder.BuildContextFromDocker(dockerContext)})
	if err != nil {
		return nil, w.Wrapf(err, "cannot call build")
	}
	if resp == nil {
		return nil, w.Wrapf(fmt.Errorf("builder returned nil response without error"), "gRPC contract violation")
	}

	if resp.State != nil && resp.State.State != builderv0.BuildStatus_SUCCESS {
		return nil, w.NewError("call to build failed")
	}

	err = b.outputPropertyForBuild.Set(ctx, &BuilderBuildOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for build")
	}

	outputProperty, err := b.outputPropertyForBuild.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for build")
	}

	if push.Load() && resp.Result != nil {
		if buildResult := dockerBuildResult(resp.Result); buildResult != nil {
			w.Info("Pushing docker image", wool.Field("result", resp.Result))
			for _, im := range buildResult.Images {
				cmd := exec.Command("docker", "push", im)
				err := cmd.Run()
				if err != nil {
					return nil, w.Wrapf(err, "cannot push docker image")
				}
			}
		}
	}
	return outputProperty, nil
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

var push atomic.Bool

func SetBuilderPush() {
	push.Store(true)
}

func (b *Builder) Unique() string {
	return b.instance.Unique()
}

var dryRun bool

func SetDryRun(d bool) {
	dryRun = d
}
