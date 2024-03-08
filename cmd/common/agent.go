package common

import (
	"context"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
)

func GetAgent(ctx context.Context, input string) (*configurations.Agent, error) {
	w := wool.Get(ctx).In("getAgent")
	agent, err := configurations.ParseAgent(ctx, configurations.ServiceAgent, input)
	if err != nil {
		return nil, w.Wrapf(err, "cannot parse agent")
	}

	// Pin to latest if needed
	if agent.Version == "latest" {
		err = manager.PinToLatestRelease(ctx, agent)
		if err != nil {
			return nil, w.Wrapf(err, "cannot pin to latest release")
		}
	}

	// Download the agent if required
	if !manager.Downloaded(agent) {
		err = manager.Download(ctx, agent)
		if err != nil {
			return nil, w.Wrapf(err, "cannot download agent")
		}
	}
	return agent, nil
}
