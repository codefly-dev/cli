package services

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"github.com/codefly-dev/core/agents/manager"
	resources "github.com/codefly-dev/core/resources"
	plugin "github.com/hashicorp/go-plugin"

	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents"
	coreservices "github.com/codefly-dev/core/agents/services"
)

var agentsCache map[string]*coreservices.ServiceAgent
var agentsPid map[string]int

func init() {
	agentsCache = make(map[string]*coreservices.ServiceAgent)
	agentsPid = make(map[string]int)
}

func LoadAgent(ctx context.Context, agent *resources.Agent) (*coreservices.ServiceAgent, error) {
	if agent == nil {
		return nil, fmt.Errorf("service cannot be nil")
	}
	w := wool.Get(ctx).In("services.LoadAgent", wool.Field("agent", agent.Name))
	w.Debug("loading service agent")
	if agent.Version == "latest" {
		err := manager.PinToLatestRelease(ctx, agent)
		if err != nil {
			return nil, w.Wrap(err)
		}
	}
	if loaded, ok := agentsCache[agent.Unique()]; ok {
		return loaded, nil
	}

	loaded, process, err := manager.Load[coreservices.ServiceAgentContext, coreservices.ServiceAgent](
		ctx,
		agent.Of(resources.ServiceAgent),
		agent.Unique())
	if err != nil {
		return nil, w.Wrap(err)
	}
	agentsPid[agent.Unique()] = process.PID

	loaded.Agent = agent
	loaded.ProcessInfo = process

	agentsCache[agent.Unique()] = loaded
	return loaded, nil
}

// NewServiceAgent binds the agent implementation to the agent
func NewServiceAgent(conf *resources.Agent, service coreservices.Agent) agents.AgentImplementation {
	return agents.AgentImplementation{
		Configuration: conf,
		Agent:         &coreservices.ServiceAgentGRPC{Service: service},
	}
}

func ClearAgents() {
	plugin.CleanupClients()
	for _, loaded := range []map[string]int{agentsPid, runtimesPid, buildersPid} {
		for _, pid := range loaded {
			process, err := os.FindProcess(pid)
			if err != nil {
				continue
			}

			err = process.Signal(syscall.SIGTERM)
			if err != nil {
				fmt.Println("cannot kill process", pid, err)
			}
		}
	}
}
