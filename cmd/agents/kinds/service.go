package kinds

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/resources"
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
	return agentInfo(ctx, resources.ServiceAgent, "Service", input)
}

func init() {
	ServiceCmd.Flags().StringVar(&agentInput, "agent", "", "Instance agentInput to get started")
}
