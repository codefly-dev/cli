package services

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/runners"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/cli/pkg/cli"
	clicommunicate "github.com/codefly-dev/cli/pkg/cli/communicate"
	"github.com/codefly-dev/core/agents/communicate"

	basev0 "github.com/codefly-dev/core/generated/go/base/v0"

	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"

	"github.com/codefly-dev/core/configurations"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
)

type ProcessInfo struct {
	AgentPID int
}

type Instance struct {
	*configurations.Service

	Agent services.Agent
	Info  *agentv0.AgentInformation

	Builder *BuilderInstance
	Runtime *RuntimeInstance

	ProcessInfo
	Capabilities []*agentv0.Capability
}

type BuilderInstance struct {
	*configurations.Service
	services.Builder
}

type RuntimeInstance struct {
	*configurations.Service
	services.Runtime
}

// Builder methods

func (instance *BuilderInstance) Load(ctx context.Context) (*builderv0.LoadResponse, error) {
	init := &builderv0.LoadRequest{
		Debug: wool.IsDebug(),
		Identity: &basev0.ServiceIdentity{
			Name:        instance.Service.Name,
			Application: instance.Service.Application,
			Domain:      instance.Service.Domain,
			Namespace:   instance.Service.Namespace,
			Location:    instance.Service.Dir(),
		},
	}
	return instance.Builder.Load(ctx, init)

}

func (instance *BuilderInstance) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	w := wool.Get(ctx).In("BuilderInstance::Create", wool.NameField(instance.Service.Unique()))
	err := communicate.Do[builderv0.CreateRequest](ctx, instance.Builder, clicommunicate.NewPrompt())
	if err != nil {
		return &builderv0.CreateResponse{Status: &builderv0.CreateStatus{Status: builderv0.CreateStatus_ERROR, Message: err.Error()}},
			w.Wrapf(err, "cannot communicate")
	}
	cli.Header(1, "Going to work!")
	s := cli.Spinner()
	defer s.Stop()
	// Start the spinner
	defer s.Stop() //

	return instance.Builder.Create(ctx, req)
}

func (instance *BuilderInstance) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	w := wool.Get(ctx).In("BuilderInstance::Sync", wool.NameField(instance.Service.Unique()))
	// Communicate always
	err := communicate.Do[builderv0.SyncRequest](ctx, instance.Builder, clicommunicate.NewPrompt())
	if err != nil {
		return &builderv0.SyncResponse{Status: &builderv0.SyncStatus{Status: builderv0.SyncStatus_ERROR, Message: err.Error()}},
			w.Wrapf(err, "cannot communicate")
	}
	return instance.Builder.Sync(ctx, req)
}

// Runner methods

func (instance *RuntimeInstance) Load(ctx context.Context) (*runtimev0.LoadResponse, error) {
	w := wool.Get(ctx).In("RuntimeInstance::Load", wool.NameField(instance.Service.Unique()))
	w.Debug("sending load")
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

	info, err := agent.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot get agent information")
	}

	instance.Capabilities = info.Capabilities

	instance.Info = info
	return instance, nil
}

func (instance *Instance) LoadBuilder(ctx context.Context) error {
	w := wool.Get(ctx).In("ServiceInstance::LoadBuilder", wool.NameField(instance.Service.Unique()))
	err := instance.CheckCapabilities(agentv0.Capability_BUILDER)
	if err != nil {
		return w.Wrapf(err, "missing builder capability")
	}
	builder, err := services.LoadBuilder(ctx, instance.Service)
	if err != nil {
		return w.Wrapf(err, "cannot load builder")
	}
	instance.Builder = &BuilderInstance{Service: instance.Service, Builder: builder}
	return nil
}

func (instance *Instance) LoadRuntime(ctx context.Context) error {
	w := wool.Get(ctx).In("ServiceInstance::LoadRuntime", wool.NameField(instance.Service.Unique()))
	err := instance.CheckCapabilities(agentv0.Capability_RUNTIME)
	if err != nil {
		return w.Wrapf(err, "missing builder capability")
	}
	err = runners.CheckForRuntimes(ctx, instance.Info.RuntimeRequirements)
	if err != nil {
		return w.Wrapf(err, "missing some runtimes")
	}
	runtime, err := services.LoadRuntime(ctx, instance.Service)
	if err != nil {
		return w.Wrapf(err, "cannot load runtime")
	}
	instance.Runtime = &RuntimeInstance{Service: instance.Service, Runtime: runtime}
	return nil
}

func (instance *Instance) CheckCapabilities(capability agentv0.Capability_Type) error {
	for _, c := range instance.Capabilities {
		if c.Type == capability {
			return nil
		}
	}
	return fmt.Errorf("missing capability %v", capability)
}

type AgentUpdate struct {
	Name string
	From string
	To   string
}

type UpdateInformation struct {
	*AgentUpdate
}

func UpdateAgent(ctx context.Context, service *configurations.Service) (*UpdateInformation, error) {
	w := wool.Get(ctx).In("ServiceInstance::Update")
	agentVersion := service.Agent.Version
	info := &UpdateInformation{}
	// Fetch the latest agent version
	err := manager.PinToLatestRelease(ctx, service.Agent)
	if err != nil {
		return nil, w.Wrap(err)
	}
	if service.Agent.Version != agentVersion {
		info.AgentUpdate = &AgentUpdate{Name: service.Agent.Name, From: agentVersion, To: service.Agent.Version}
	}
	err = service.Save(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return info, nil
}
