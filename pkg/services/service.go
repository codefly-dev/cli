package services

import (
	"context"

	"github.com/codefly-dev/core/agents/services"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/wool"

	clicommunicate "github.com/codefly-dev/cli/pkg/cli/communicate"
	"github.com/codefly-dev/core/agents/communicate"

	basev1 "github.com/codefly-dev/core/generated/go/base/v1"

	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"

	"github.com/codefly-dev/core/configurations"
	v1agent "github.com/codefly-dev/core/generated/go/services/agent/v1"
	factoryv1 "github.com/codefly-dev/core/generated/go/services/factory/v1"
)

type ProcessInfo struct {
	AgentPID int
}

type Instance struct {
	*configurations.Service
	Agent   services.Agent
	Factory *FactoryInstance
	Runtime *RuntimeInstance
	ProcessInfo
}

type FactoryInstance struct {
	*configurations.Service
	services.Factory
}

type RuntimeInstance struct {
	*configurations.Service
	services.Runtime
}

// Factory methods

func (instance *FactoryInstance) Load(ctx context.Context) (*factoryv1.LoadResponse, error) {
	init := &factoryv1.LoadRequest{
		Debug: wool.IsDebug(),
		Identity: &basev1.ServiceIdentity{
			Name:        instance.Name,
			Application: instance.Application,
			Domain:      instance.Domain,
			Namespace:   instance.Namespace,
			Location:    instance.Dir(),
		},
	}
	return instance.Factory.Load(ctx, init)

}

func (instance *FactoryInstance) Create(ctx context.Context, req *factoryv1.CreateRequest) (*factoryv1.CreateResponse, error) {
	w := wool.Get(ctx).In("FactoryInstance::Create", wool.NameField(instance.Unique()))
	err := communicate.Do[factoryv1.CreateRequest](ctx, instance.Factory, clicommunicate.NewPrompt())
	if err != nil {
		return &factoryv1.CreateResponse{Status: &factoryv1.CreateStatus{Status: factoryv1.CreateStatus_ERROR, Message: err.Error()}},
			w.Wrapf(err, "cannot communicate")
	}
	return instance.Factory.Create(ctx, req)
}

func (instance *FactoryInstance) Sync(ctx context.Context, req *factoryv1.SyncRequest) (*factoryv1.SyncResponse, error) {
	w := wool.Get(ctx).In("FactoryInstance::Sync", wool.NameField(instance.Unique()))
	// Communicate always
	err := communicate.Do[factoryv1.SyncRequest](ctx, instance.Factory, clicommunicate.NewPrompt())
	if err != nil {
		return &factoryv1.SyncResponse{Status: &factoryv1.SyncStatus{Status: factoryv1.SyncStatus_ERROR, Message: err.Error()}},
			w.Wrapf(err, "cannot communicate")
	}
	return instance.Factory.Sync(ctx, req)
}

// Runtime methods

func (instance *RuntimeInstance) Load(ctx context.Context) (*runtimev1.LoadResponse, error) {
	init := &runtimev1.LoadRequest{
		Debug: wool.IsDebug(),
		Identity: &basev1.ServiceIdentity{
			Name:        instance.Name,
			Application: instance.Application,
			Domain:      instance.Domain,
			Namespace:   instance.Namespace,
			Location:    instance.Dir(),
		},
	}
	return instance.Runtime.Load(ctx, init)
}

// Loader

func Load(ctx context.Context, service *configurations.Service) (*Instance, error) {
	w := wool.Get(ctx).In("services.Load", wool.ThisField(service))
	agent, proc, err := manager.Load[services.ServiceAgentContext, services.ServiceAgent](ctx, service.Agent, service.Unique())
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service agent")
	}
	// Init capabilities
	instance := &Instance{
		Service: service,
		Agent:   agent,
	}
	instance.ProcessInfo.AgentPID = proc.PID

	info, err := agent.GetAgentInformation(ctx, &v1agent.AgentInformationRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot get agent information")
	}

	for _, capability := range info.Capabilities {
		switch capability.Type {
		case v1agent.Capability_FACTORY:
			err = instance.LoadFactory(ctx, service)
			if err != nil {
				return nil, w.Wrapf(err, "cannot provide factory")
			}
		case v1agent.Capability_RUNTIME:
			err = instance.LoadRuntime(ctx, service)
			if err != nil {
				return nil, w.Wrapf(err, "cannot provide runtime")
			}
		}

	}
	return instance, nil
}

func (instance *Instance) LoadFactory(ctx context.Context, service *configurations.Service) error {
	w := wool.Get(ctx).In("ServiceInstance::LoadFactory", wool.NameField(service.Unique()))
	factory, err := services.LoadFactory(ctx, service)
	if err != nil {
		return w.Wrapf(err, "cannot load factory")
	}
	instance.Factory = &FactoryInstance{Service: service, Factory: factory}
	return nil
}

func (instance *Instance) LoadRuntime(ctx context.Context, service *configurations.Service) error {
	w := wool.Get(ctx).In("ServiceInstance::LoadRuntime", wool.NameField(service.Unique()))
	runtime, err := services.LoadRuntime(ctx, service)
	if err != nil {
		return w.Wrapf(err, "cannot load runtime")
	}
	instance.Runtime = &RuntimeInstance{Service: service, Runtime: runtime}
	return nil
}

func UpdateAgent(ctx context.Context, service *configurations.Service) error {
	w := wool.Get(ctx).In("ServiceInstance::Update")
	// Fetch the latest agent version
	err := manager.PinToLatestRelease(ctx, service.Agent)
	if err != nil {
		return w.Wrap(err)
	}
	err = service.Save(ctx)
	if err != nil {
		return w.Wrap(err)
	}
	return nil
}
