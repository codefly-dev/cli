package kinds

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Load a service agent and print its reported capabilities",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		return serviceInfo(ctx, agentInput)
	},
}

func serviceInfo(ctx context.Context, input string) error {
	defer services.ClearAgents()
	w := wool.Get(ctx).In("cmd.info.agentInput.service")
	ctx = w.Inject(ctx)
	if input == "" {
		return fmt.Errorf("--agent is required")
	}

	conf, err := resources.ParseAgent(ctx, resources.ServiceAgent, input)
	if err != nil {
		return fmt.Errorf("cannot parse agent: %w", err)
	}

	cli.Header(1, "Fetching information about Service Agent <%s> information", conf)

	agent, err := services.LoadAgent(ctx, conf, "")
	if err != nil {
		return fmt.Errorf("cannot load agent: %w", err)
	}
	cli.Header(2, "Successfully loaded service agent <%s>", conf)

	info, err := agent.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		return fmt.Errorf("cannot get agent information: %w", err)
	}
	fmt.Println(info)
	return nil
}

func init() {
	ServiceCmd.Flags().StringVar(&agentInput, "agent", "", "Instance agentInput to get started")
}
