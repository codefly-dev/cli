package common

import (
	"context"

	"github.com/codefly-dev/core/agents/manager"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func GetAgent(ctx context.Context, input string) (*resources.Agent, error) {
	w := wool.Get(ctx).In("getAgent")
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, input)
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
