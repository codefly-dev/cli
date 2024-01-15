package services

import (
	"context"
	"time"

	"github.com/briandowns/spinner"
	"github.com/codefly-dev/core/agents/services"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/cli/pkg/cli"
	clicommunicate "github.com/codefly-dev/cli/pkg/cli/communicate"
	"github.com/codefly-dev/core/agents/communicate"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"

	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"

	"github.com/codefly-dev/core/configurations"
	v0agent "github.com/codefly-dev/core/generated/go/services/agent/v0"
	factoryv0 "github.com/codefly-dev/core/generated/go/services/factory/v0"
)

type ProcessInfo struct {
	AgentPID int
}

type Instance struct {
	*configurations.Service

	Agent services.Agent
	Info  *v0agent.AgentInformation

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

func (instance *FactoryInstance) Load(ctx context.Context) (*factoryv0.LoadResponse, error) {
	init := &factoryv0.LoadRequest{
		Debug: wool.IsDebug(),
		Identity: &basev0.ServiceIdentity{
			Name:        instance.Service.Name,
			Application: instance.Service.Application,
			Domain:      instance.Service.Domain,
			Namespace:   instance.Service.Namespace,
			Location:    instance.Service.Dir(),
		},
	}
	return instance.Factory.Load(ctx, init)

}

func (instance *FactoryInstance) Create(ctx context.Context, req *factoryv0.CreateRequest) (*factoryv0.CreateResponse, error) {
	w := wool.Get(ctx).In("FactoryInstance::Create", wool.NameField(instance.Service.Unique()))
	err := communicate.Do[factoryv0.CreateRequest](ctx, instance.Factory, clicommunicate.NewPrompt())
	if err != nil {
		return &factoryv0.CreateResponse{Status: &factoryv0.CreateStatus{Status: factoryv0.CreateStatus_ERROR, Message: err.Error()}},
			w.Wrapf(err, "cannot communicate")
	}
	cli.Header(1, "Going to work!")
	s := spinner.New(spinner.CharSets[11], 100*time.Millisecond) // Use different character sets and duration
	s.Start()                                                    // Start the spinner
	defer s.Stop()                                               //

	return instance.Factory.Create(ctx, req)
}

func (instance *FactoryInstance) Sync(ctx context.Context, req *factoryv0.SyncRequest) (*factoryv0.SyncResponse, error) {
	w := wool.Get(ctx).In("FactoryInstance::Sync", wool.NameField(instance.Service.Unique()))
	// Communicate always
	err := communicate.Do[factoryv0.SyncRequest](ctx, instance.Factory, clicommunicate.NewPrompt())
	if err != nil {
		return &factoryv0.SyncResponse{Status: &factoryv0.SyncStatus{Status: factoryv0.SyncStatus_ERROR, Message: err.Error()}},
			w.Wrapf(err, "cannot communicate")
	}
	return instance.Factory.Sync(ctx, req)
}

// Runtime methods

func (instance *RuntimeInstance) Load(ctx context.Context) (*runtimev0.LoadResponse, error) {
	init := &runtimev0.LoadRequest{
		Debug: wool.IsDebug(),
		Identity: &basev0.ServiceIdentity{
			Name:        instance.Service.Name,
			Application: instance.Service.Application,
			Domain:      instance.Service.Domain,
			Namespace:   instance.Service.Namespace,
			Location:    instance.Service.Dir(),
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

	info, err := agent.GetAgentInformation(ctx, &v0agent.AgentInformationRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot get agent information")
	}

	for _, capability := range info.Capabilities {
		switch capability.Type {
		case v0agent.Capability_FACTORY:
			err = instance.LoadFactory(ctx, service)
			if err != nil {
				return nil, w.Wrapf(err, "cannot provide factory")
			}
		case v0agent.Capability_RUNTIME:
			err = instance.LoadRuntime(ctx, service)
			if err != nil {
				return nil, w.Wrapf(err, "cannot provide runtime")
			}
		}
	}
	instance.Info = info
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
