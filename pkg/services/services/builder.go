package services

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/manager"
	coreservices "github.com/codefly-dev/core/agents/services"

	"github.com/codefly-dev/core/configurations"
)

var buildersCache map[string]int
var buildersPid map[string]int

func init() {
	buildersCache = make(map[string]int)
	buildersPid = make(map[string]int)
}

func LoadBuilder(ctx context.Context, conf *configurations.Service) (*coreservices.BuilderAgent, error) {
	w := wool.Get(ctx).In("services.LoadBuilder", wool.ThisField(conf))
	if buildersCache[conf.Unique()] > 0 {
		return nil, fmt.Errorf("already loaded")
	}
	buildersCache[conf.Unique()]++

	if conf == nil {
		return nil, fmt.Errorf("conf cannot be nil")
	}
	if conf.Agent == nil {
		return nil, w.NewError("agent cannot be nil")
	}
	builder, process, err := manager.Load[coreservices.ServiceBuilderAgentContext, coreservices.BuilderAgent](ctx, conf.Agent.Of(configurations.BuilderServiceAgent), conf.Unique())
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service builder conf")
	}

	buildersPid[conf.Unique()] = process.PID

	builder.Agent = conf.Agent
	builder.ProcessInfo = process
	return builder, nil
}

func NewBuilderAgent(conf *configurations.Agent, builder coreservices.Builder) agents.AgentImplementation {
	return agents.AgentImplementation{
		Configuration: conf,
		Agent:         &coreservices.BuilderAgentGRPC{Builder: builder},
	}
}
