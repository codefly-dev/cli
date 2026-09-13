package kinds

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// RunnableCmd loads a runnable language agent (runnable-python, runnable-go, …)
// and prints what it reports. The agent is resolved through the shared
// agent-kind machinery: the language is the agent's name, never a branch here.
var RunnableCmd = &cobra.Command{
	Use:   "runnable",
	Short: "Load a runnable agent and print its reported capabilities",
	Long: `Load a runnable language agent over gRPC and print what it reports about itself.

Examples:
  codefly agent info runnable --agent=python:0.0.1`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		return runnableInfo(ctx, runnableAgentInput)
	},
}

func runnableInfo(ctx context.Context, input string) error {
	return agentInfo(ctx, resources.RunnableAgent, "Runnable", input)
}

func init() {
	RunnableCmd.Flags().StringVar(&runnableAgentInput, "agent", "", "Runnable agent to inspect (name:version)")
}
