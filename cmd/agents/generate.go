package agents

import (
	"github.com/codefly-dev/core/agents/generator"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// GenerateCmd represents the run command
var GenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "agentInput generation",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		w := wool.Get(ctx).In("agents.GenerateCmd")
		p := configurations.SolveDir(service)
		logger.Debugf("generating service agentInput from existing source at: %s", p)
		err := generator.GenerateServiceTemplate(ctx, p)
		shared.ExitOnError(err, "cannot create service")
	},
}

var service string

func init() {
	GenerateCmd.PersistentFlags().StringVar(&service, "service", "", "NewDir to the code to turn into library")
}
