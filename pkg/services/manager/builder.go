package manager

import (
	"context"

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

	// API
	endpoints       []*basev0.Endpoint
	networkMappings []*basev0.NetworkMapping

	// For "callbacks"
	playbook *Playbook

	// State
	sharedState *StateManager

	// Requires
	requires []string

	// outputProperty managers
	isStarted bool

	outputPropertyForLoad  *BuilderLoadManager
	outputPropertyForInit  *BuilderInitManager
	outputPropertyForBuild *BuilderBuildManager
	outputPropertyForSync  *BuilderSyncManager
}

func NewBuilder(ctx context.Context, instance *services.Instance, playbook *Playbook, sharedState *StateManager) (*Builder, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(instance))
	w.Debug("new")
	builder := &Builder{
		instance: instance,

		playbook: playbook,

		sharedState: sharedState,

		outputPropertyForLoad:  NewBuilderLoadManager(instance.Unique()),
		outputPropertyForInit:  NewBuilderInitManager(instance.Unique()),
		outputPropertyForBuild: NewBuilderBuildManager(instance.Unique()),
		outputPropertyForSync:  NewBuilderSyncManager(instance.Unique()),
	}
	return builder, nil
}

func (builder *Builder) Load(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(builder.instance.Service))

	resp, err := builder.instance.Builder.Load(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("loaded",
		wool.Field("endpoints", configurations.MakeEndpointSummary(resp.Endpoints)))

	builder.endpoints = resp.Endpoints

	err = builder.outputPropertyForLoad.Set(ctx, &BuilderLoadOutput{Endpoints: resp.Endpoints})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for load")
	}

	outputProperty, err := builder.outputPropertyForLoad.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for load")
	}

	err = builder.sharedState.RecordEndpoints(ctx, builder.instance.Service, resp.Endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record endpoints")
	}

	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
	return outputProperty, nil
}

func (builder *Builder) Init(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(builder.instance.Service))
	w.Debug("init")
	// Build the request
	env, err := builder.playbook.world.Env.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	dependenciesEndpoints, err := builder.sharedState.GetDependenciesEndpoints(ctx, builder.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	infos, err := builder.sharedState.GetProviderInfos(ctx, builder.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	// Get all the shared provider info from the dependents

	resp, err := builder.instance.Builder.Init(ctx, &builderv0.InitRequest{
		Environment:           env,
		ProviderInfos:         infos,
		DependenciesEndpoints: dependenciesEndpoints,
	})
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	if resp.State.State != builderv0.InitStatus_SUCCESS {
		return nil, w.NewError("service instance is not ready")
	}

	err = builder.outputPropertyForInit.Set(ctx, &BuilderInitOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for init")
	}

	outputProperty, err := builder.outputPropertyForInit.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for init")
	}

	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
	return outputProperty, nil
}

func generateDNSNetworkMappings(ctx context.Context, endpoints []*basev0.Endpoint) ([]*basev0.NetworkMapping, error) {
	w := wool.Get(ctx).In("service.NewRunner")
	w.Debug("endpoints", wool.NullableField("got", configurations.MakeEndpointSummary(endpoints)))

	pm, err := network.NewServiceDNSManager(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create default endpoint")
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
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(builder.instance.Service))
	w.Debug("init")
	// Build the request

	dependenciesEndpoints, err := builder.sharedState.GetDependenciesEndpoints(ctx, builder.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	networkMappings, err := generateDNSNetworkMappings(ctx, dependenciesEndpoints)
	// Get all the shared provider info from the dependents

	resp, err := builder.instance.Builder.Build(ctx, &builderv0.BuildRequest{NetworkMappings: networkMappings})
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	w.Debug("BUILD PROTO BASE")
	if resp.State != nil && resp.State.State != builderv0.BuildStatus_SUCCESS {
		return nil, w.NewError("service instance is not ready")
	}

	err = builder.outputPropertyForBuild.Set(ctx, &BuilderBuildOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for build")
	}

	outputProperty, err := builder.outputPropertyForBuild.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for build")
	}

	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
	return outputProperty, nil
}

func (builder *Builder) Sync(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("service.NewBuilder", wool.ThisField(builder.instance.Service))
	w.Debug("sync")

	// Build the request
	resp, err := builder.instance.Builder.Sync(ctx, &builderv0.SyncRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot start service instance")
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

	w.Debug("outputProperty", wool.Field("outputProperty", outputProperty))
	builder.isStarted = true
	return outputProperty, nil
}

func (builder *Builder) Unique() string {
	return builder.instance.Service.Unique()
}
