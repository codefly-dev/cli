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
	Short: "Get service information",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		serviceInfo(ctx, agentInput)
	},
}

func serviceInfo(ctx context.Context, input string) {
	defer services.ClearAgents()
	w := wool.Get(ctx).In("cmd.info.agentInput.service")
	ctx = w.Inject(ctx)

	conf, err := resources.ParseAgent(ctx, resources.ServiceAgent, input)
	cli.ExitOnError(err, "Cannot parse agentInput")

	cli.Header(1, "Fetching information about Service Agent <%s> information", conf)

	agent, err := services.LoadAgent(ctx, conf)
	cli.ExitOnError(err, "Cannot load agentInput")
	cli.Header(2, "Successfully loaded service agent <%s>", conf)

	info, err := agent.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	cli.ExitOnError(err, "Cannot get agent information")
	fmt.Println(info)

}

func init() {
	ServiceCmd.Flags().StringVar(&agentInput, "agent", "", "Instance agentInput to get started")
}
