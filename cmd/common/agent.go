package common

import (
	"context"

	"github.com/codefly-dev/core/agents/manager"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func GetAgent(ctx context.Context, input string) (*resources.Agent, error) {
	return GetAgentWithKind(ctx, resources.ServiceAgent, input)
}

func GetApplicationAgent(ctx context.Context, input string) (*resources.Agent, error) {
	return GetAgentWithKind(ctx, resources.ApplicationAgent, input)
}

func GetModuleAgent(ctx context.Context, input string) (*resources.Agent, error) {
	return GetAgentWithKind(ctx, resources.ModuleAgent, input)
}

func GetAgentWithKind(ctx context.Context, kind resources.AgentKind, input string) (*resources.Agent, error) {
	w := wool.Get(ctx).In("getAgentWithKind")
	agent, err := resources.ParseAgent(ctx, kind, input)
	if err != nil {
		return nil, w.Wrapf(err, "cannot parse agent")
	}

	// Resolve "latest" (local-first, falls back to GitHub).
	if _, err := manager.ResolveLatest(ctx, agent); err != nil {
		return nil, w.Wrapf(err, "cannot resolve latest version")
	}

	// Download the agent if required
	downloaded, err := manager.Downloaded(ctx, agent)
	if err != nil {
		return nil, w.Wrapf(err, "cannot check if agent is downloaded")
	}
	if !downloaded {
		err = manager.Download(ctx, agent)
		if err != nil {
			return nil, w.Wrapf(err, "cannot download agent")
		}
	}
	return agent, nil
}
