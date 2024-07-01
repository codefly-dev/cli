package manager

import (
	"context"
	"fmt"
	"os/exec"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	resources "github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/cli/communicate"
	"github.com/codefly-dev/cli/pkg/deployment"
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
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(instance))
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
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(b.instance.Service))

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

	err = b.world.SharedState.RecordEndpoints(ctx, b.instance.Service, resp.Endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record endpoints")
	}

	return outputProperty, nil
}

func (b *Builder) Init(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance.Service))
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
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance.Service))

	// Build the request
	resp, err := b.instance.Builder.Sync(ctx, &builderv0.SyncRequest{}, communicate.NewPrompt())
	if err != nil {
		return nil, w.Wrapf(err, "cannot sync service instance")
	}
	if resp.State.State != builderv0.SyncStatus_SUCCESS {
		return nil, w.NewError("service instance is not started")
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
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance.Service))
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
		return &OutputProperty{UpdateWithRequiredPropagation: true}, nil
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

	if push && resp.Result != nil {
		if buildResult := resp.Result.Kind.(*builderv0.BuildResult_DockerBuildResult); buildResult != nil {
			w.Info("Pushing docker image", wool.Field("result", resp.Result))
			for _, im := range buildResult.DockerBuildResult.Images {
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

var push bool

func SetBuilderPush() {
	push = true
}

func (b *Builder) Deploy(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance.Service))
	w.Debug("Deploy")

	env, err := b.world.Env.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	conf, err := b.world.ConfigurationManager.GetServiceConfiguration(ctx, b.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get ConfigurationManager information")
	}

	dependenciesConfigurations, err := b.world.SharedState.GetDependentConfigurationsFor(ctx, b.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get configuration")
	}

	networkMappings, err := b.world.NetworkManager.GenerateNetworkMappings(ctx, b.world.Env, b.world.Workspace, b.instance.Service, b.endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings for service endpoints")
	}

	err = b.world.SharedState.RecordNetworkMappings(ctx, b.instance.Service, networkMappings)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record network mappings")
	}

	dependenciesNetworkMappings, err := b.world.SharedState.GetDependenciesNetworkMappings(ctx, b.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	namespace, err := b.world.NetworkManager.GetNamespace(ctx, b.world.Env, b.world.Workspace, b.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get namespace")
	}

	// Build the request
	dockerContext, err := builder.DockerBuildContext(ctx, b.world.Workspace)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create build context")
	}

	// Public endpoints need Load Balancer
	//withLoadBalancer := env.LoadBalancer != "" && resources.HasPublicEndpoints(b.endpoints)
	deploy, err := deployment.GetKubernetesDeployment(ctx, dockerContext, b.world.Workspace, b.instance.Service, b.world.Env, namespace, false)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	// Build the request
	w.Debug("deployments", wool.Field("deployments", deploy))

	resp, err := b.instance.Builder.Deploy(ctx, &builderv0.DeploymentRequest{
		Environment:                 env,
		Deployment:                  deploy,
		Configuration:               conf,
		DependenciesConfigurations:  dependenciesConfigurations,
		NetworkMappings:             networkMappings,
		DependenciesNetworkMappings: dependenciesNetworkMappings,
	})
	if err != nil {
		return nil, w.Wrapf(err, "cannot deploy service instance")
	}

	if resp.State != nil && resp.State.State != builderv0.DeploymentStatus_SUCCESS {
		return nil, w.NewError("service instance is not started")
	}

	err = b.world.ConfigurationManager.ExposeConfiguration(ctx, b.instance.Service, resp.Configuration)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record shared configuration configurations")
	}

	err = b.outputPropertyForSync.Set(ctx, &BuilderSyncOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for deploy")
	}

	outputProperty, err := b.outputPropertyForSync.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for deploy")
	}

	// Deploy

	if dryRun {
		return outputProperty, nil
	}
	if resp.Deployment == nil {
		return outputProperty, nil
	}
	switch v := resp.Deployment.Kind.(type) {
	case *builderv0.DeploymentOutput_Kubernetes:
		if v.Kubernetes.Kind == builderv0.KubernetesDeploymentOutput_Kustomize {
			err = b.KustomizeApply(ctx, b.instance.Service)
			if err != nil {
				return nil, w.Wrapf(err, "cannot apply kustomize")
			}
		}
	default:
		return nil, w.NewError("not supported deployment found")
	}
	return outputProperty, nil

}

func (b *Builder) KustomizeApply(ctx context.Context, service *resources.Service) error {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance.Service))
	dir := deployment.Dir(ctx, b.world.Workspace)
	dir = fmt.Sprintf("%s/modules/%s/services/%s/overlays/%s", dir, service.Module, service.Name, b.world.Env.Name)
	err := deployment.KustomizeApply(ctx, service, b.world.Env, dir)
	if err != nil {
		return w.Wrapf(err, "cannot apply kustomize")
	}
	return nil
}

func (b *Builder) Unique() string {
	return b.instance.Service.Unique()
}

var dryRun bool

func SetDryRun(d bool) {
	dryRun = d
}
