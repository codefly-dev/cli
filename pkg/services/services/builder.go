package services

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/agents"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/manager"
	coreservices "github.com/codefly-dev/core/agents/services"
)

var buildersCache map[string]*coreservices.BuilderAgent
var buildersPid map[string]int

func init() {
	buildersCache = make(map[string]*coreservices.BuilderAgent)
	buildersPid = make(map[string]int)
}

func LoadBuilder(ctx context.Context, conf *resources.Service) (*coreservices.BuilderAgent, error) {
	w := wool.Get(ctx).In("services.LoadBuilder", wool.ThisField(conf))

	if conf == nil {
		return nil, fmt.Errorf("conf cannot be nil")
	}
	if conf.Agent == nil {
		return nil, w.NewError("agent cannot be nil")
	}

	if builder, ok := buildersCache[conf.Unique()]; ok {
		return builder, nil
	}

	builder, process, err := manager.Load[coreservices.ServiceBuilderAgentContext, coreservices.BuilderAgent](ctx, conf.Agent.Of(resources.BuilderServiceAgent), conf.Unique())
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service builder conf")
	}

	buildersPid[conf.Unique()] = process.PID

	builder.Agent = conf.Agent
	builder.ProcessInfo = process

	w.Debug("loaded builder", wool.Field("builder-pid", process.PID))

	buildersCache[conf.Unique()] = builder
	return builder, nil
}

func NewBuilderAgent(conf *resources.Agent, builder coreservices.Builder) agents.AgentImplementation {
	return agents.AgentImplementation{
		Configuration: conf,
		Agent:         &coreservices.BuilderAgentGRPC{Builder: builder},
	}
}
