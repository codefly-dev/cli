package kinds

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
)

var (
	agentInput         string
	runnableAgentInput string
)

// agentInfo loads an agent of the given kind and prints what it reports about
// itself. Resolution, download and the gRPC handshake are the same for every
// kind; only the registered kind differs.
func agentInfo(ctx context.Context, kind resources.AgentKind, input string) error {
	defer services.ClearAgents()
	w := wool.Get(ctx).In("cmd.info.agentInput", wool.Field("kind", string(kind)))
	ctx = w.Inject(ctx)
	if input == "" {
		return fmt.Errorf("--agent is required")
	}

	conf, err := resources.ParseAgent(ctx, kind, input)
	if err != nil {
		return fmt.Errorf("cannot parse agent: %w", err)
	}

	cli.Header(1, "Fetching information about <%s> agent <%s>", kind, conf)

	agent, err := services.LoadAgent(ctx, conf, "")
	if err != nil {
		return fmt.Errorf("cannot load agent: %w", err)
	}
	cli.Header(2, "Successfully loaded <%s> agent <%s>", kind, conf)

	info, err := agent.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		return fmt.Errorf("cannot get agent information: %w", err)
	}
	fmt.Println(info)
	return nil
}
