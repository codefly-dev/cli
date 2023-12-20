package kinds

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Get service information",
	Run: func(cmd *cobra.Command, args []string) {
		serviceInfo(agentInput)
	},
}

func serviceInfo(input string) {
	ctx, done := common.NewContext()
	defer done()

	defer agents.ClearAgents()
	w := wool.Get(ctx).In("cmd.info.agentInput.service")

	conf, err := configurations.ParseAgent(w.Context(), configurations.ServiceAgent, input)
	cli.ExitOnError(err, "Cannot parse agentInput")

	cli.Header(1, "Fetching information about Service Agent <{{.Name}}> information", conf)

	agent, err := services.LoadAgent(w.Context(), conf)
	cli.ExitOnError(err, "Cannot load agentInput")
	cli.Header(2, "Successfully loaded service agent <{{.Name}}>", conf)

	info, err := agent.GetAgentInformation(w.Context(), &agentv1.AgentInformationRequest{})
	cli.ExitOnError(err, "Cannot get agent information")
	fmt.Println(info)

}

func init() {
	ServiceCmd.Flags().StringVar(&agentInput, "agent", "", "Instance agentInput to get started")
}
