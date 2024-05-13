package services

import (
	"context"

	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/manager"
	coreservices "github.com/codefly-dev/core/agents/services"

	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
)

/*
Loader
*/

var runtimesCache map[string]*coreservices.RuntimeAgent
var runtimesPid map[string]int

func init() {
	runtimesCache = make(map[string]*coreservices.RuntimeAgent)
	runtimesPid = make(map[string]int)
}

func LoadRuntime(ctx context.Context, service *resources.Service) (*coreservices.RuntimeAgent, error) {
	w := wool.Get(ctx).In("services.LoadRuntime", wool.ThisField(service))
	if service == nil || service.Agent == nil {
		return nil, w.NewError("agent cannot be nil")
	}

	if runtime, ok := runtimesCache[service.Unique()]; ok {
		return runtime, nil
	}

	runtime, process, err := manager.Load[coreservices.ServiceRuntimeAgentContext, coreservices.RuntimeAgent](
		ctx,
		service.Agent.Of(resources.RuntimeServiceAgent),
		service.Unique())
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service runtime agent")
	}

	runtimesPid[service.Unique()] = process.PID

	runtime.Agent = service.Agent
	runtime.ProcessInfo = process

	w.Debug("loaded runtime", wool.Field("runtime-pid", process.PID))

	runtimesCache[service.Unique()] = runtime

	return runtime, nil
}

type InformationStatus struct {
	Load  *runtimev0.LoadStatus
	Init  *runtimev0.InitStatus
	Start *runtimev0.StartStatus

	DesiredState *runtimev0.DesiredState
}
