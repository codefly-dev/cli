package manager

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"

	"github.com/codefly-dev/cli/pkg/services/network"
	"github.com/codefly-dev/cli/pkg/services/services"

	"github.com/codefly-dev/core/configurations"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/wool"
)

/*
Builder is a wrapper around a builder service instance to fit the outputProperty interface
*/
type Builder struct {
	instance *services.Instance

	builderContext *builderv0.BuildContext

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
}

func NewBuilder(ctx context.Context, instance *services.Instance, world *World) (*Builder, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(instance))
	w.Debug("new builder")
	builder := &Builder{
		instance: instance,

		world: world,

		outputPropertyForLoad:  NewBuilderLoadManager(instance.Unique()),
		outputPropertyForInit:  NewBuilderInitManager(instance.Unique()),
		outputPropertyForBuild: NewBuilderBuildManager(instance.Unique()),
		outputPropertyForSync:  NewBuilderSyncManager(instance.Unique()),
	}
	return builder, nil
}

func (builder *Builder) Load(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(builder.instance.Service))

	// Build the request
	env, err := builder.world.Env.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "cannot get env")
	}

	resp, err := builder.instance.Builder.Load(ctx, env)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder instance")
	}

	w.Debug("loaded", wool.Field("endpoints", configurations.MakeEndpointSummary(resp.Endpoints)))

	builder.endpoints = resp.Endpoints

	err = builder.outputPropertyForLoad.Set(ctx, &BuilderLoadOutput{Endpoints: resp.Endpoints})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for load")
	}

	outputProperty, err := builder.outputPropertyForLoad.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for load")
	}

	err = builder.world.SharedState.RecordEndpoints(ctx, builder.instance.Service, resp.Endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record endpoints")
	}

	return outputProperty, nil
}

func (builder *Builder) Init(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(builder.instance.Service))
	w.Focus("Init")

	dependenciesEndpoints, err := builder.world.SharedState.GetDependenciesEndpoints(ctx, builder.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get depednencies")
	}

	infos, err := builder.world.SharedState.GetProviderInfos(ctx, builder.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get provider")
	}

	networkMappings, err := builder.generateDNSNetworkMappings(ctx, builder.endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings")
	}

	resp, err := builder.instance.Builder.Init(ctx, &builderv0.InitRequest{
		ProposedNetworkMappings: networkMappings,
		ProviderInfos:           infos,
		DependenciesEndpoints:   dependenciesEndpoints,
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

	err = builder.world.SharedState.RecordNetworkMappings(ctx, builder.instance.Service, resp.NetworkMappings)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record network mappings")
	}

	err = builder.outputPropertyForInit.Set(ctx, &BuilderInitOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for init")
	}

	outputProperty, err := builder.outputPropertyForInit.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for init")
	}

	return outputProperty, nil
}

func (builder *Builder) generateDNSNetworkMappings(ctx context.Context, endpoints []*basev0.Endpoint) ([]*basev0.NetworkMapping, error) {
	w := wool.Get(ctx).In("service.NewRunner", wool.ThisField(builder.instance.Service))
	w.Debug("endpoints", wool.NullableField("got", configurations.MakeEndpointSummary(endpoints)))
	pm, err := network.NewServiceDNSManager(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create network manager")
	}
	// We gather public endpoints URL -- from provider info
	info, err := builder.world.Provider.GetProjectProviderInformation(ctx, "dns")
	if err == nil {
		w.Debug("provider informations", wool.Field("got", info.Data))
		dns := map[string]string{}
		for _, endpoint := range endpoints {
			if endpoint.Visibility == configurations.VisibilityPublic {
				e := configurations.EndpointFromProto(endpoint)
				dns[e.Unique()] = info.Data[e.ServiceUnique()]
			}
		}
		w.Debug("dns", wool.Field("got", dns))
		pm.WithExternalDNS(info.Data)
	}
	for _, endpoint := range endpoints {
		w.Debug("exposing", wool.Field("destination", configurations.EndpointDestination(endpoint)))
		err = pm.Expose(endpoint)
		if err != nil {
			return nil, w.Wrapf(err, "cannot add grpc endpoint to network manager")
		}
	}
	err = pm.Reserve(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot reserve ports")
	}
	networkMappings, err := pm.NetworkMapping(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create network mapping")
	}
	w.Debug("network mappings", wool.Field("mappings", configurations.MakeNetworkMappingSummary(networkMappings)))
	return networkMappings, nil
}

func (builder *Builder) Build(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(builder.instance.Service))
	w.Focus("Build")

	// Build the request
	resp, err := builder.instance.Builder.Build(ctx, &builderv0.BuildRequest{BuildContext: builder.builderContext})
	if err != nil {
		return nil, w.Wrapf(err, "cannot call build")
	}

	if resp.State != nil && resp.State.State != builderv0.BuildStatus_SUCCESS {
		return nil, w.NewError("call to build failed")
	}

	err = builder.outputPropertyForBuild.Set(ctx, &BuilderBuildOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for build")
	}

	outputProperty, err := builder.outputPropertyForBuild.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for build")
	}

	return outputProperty, nil
}

func (builder *Builder) Sync(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(builder.instance.Service))
	w.Focus("Sync")

	// Build the request
	resp, err := builder.instance.Builder.Sync(ctx, &builderv0.SyncRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot sync service instance")
	}
	if resp.State.State != builderv0.SyncStatus_SUCCESS {
		return nil, w.NewError("service instance is not started")
	}

	err = builder.outputPropertyForSync.Set(ctx, &BuilderSyncOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for sync")
	}

	outputProperty, err := builder.outputPropertyForSync.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for sync")
	}

	return outputProperty, nil
}

func (builder *Builder) Deploy(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(builder.instance.Service))
	w.Focus("Deploy")

	env, err := builder.world.Env.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	networkMappings, err := builder.world.SharedState.GetNetworkMappings(ctx, builder.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	deployment, err := builder.world.Deployer.Deployment(ctx, builder.world.Project, builder.world.Env)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	// Build the request
	w.Debug("deployments", wool.Field("deployments", deployment))
	resp, err := builder.instance.Builder.Deploy(ctx, &builderv0.DeploymentRequest{
		Environment:     env,
		BuildContext:    builder.builderContext,
		Deployment:      deployment,
		NetworkMappings: networkMappings,
	})
	if err != nil {
		return nil, w.Wrapf(err, "cannot deploy service instance")
	}
	if resp.State != nil && resp.State.State != builderv0.DeploymentStatus_SUCCESS {
		return nil, w.NewError("service instance is not started")
	}

	err = builder.outputPropertyForSync.Set(ctx, &BuilderSyncOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for deploy")
	}

	outputProperty, err := builder.outputPropertyForSync.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for deploy")
	}
	return outputProperty, nil

}

func (builder *Builder) Unique() string {
	return builder.instance.Service.Unique()
}

func (builder *Builder) SetBuildContext(builderContext *builderv0.BuildContext) {
	builder.builderContext = builderContext
}
