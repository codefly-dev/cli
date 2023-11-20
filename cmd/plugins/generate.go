package plugins

import (
	"github.com/codefly-dev/cli/pkg/plugins/generator"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// GenerateCmd represents the run command
var GenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "plugin generation for libraries",
	Run: func(cmd *cobra.Command, args []string) {
		logger := shared.NewLogger("plugins.GenerateCmd")
		// Actualize path
		p := configurations.SolveDir(service)
		logger.Debugf("generating service plugin from existing source at: %s", p)
		err := generator.GenerateServiceTemplate(p)
		shared.ExitOnError(err, "cannot create service")
	},
}

var service string

func init() {
	GenerateCmd.PersistentFlags().StringVar(&service, "service", "", "NewDir to the code to turn into library")
}
