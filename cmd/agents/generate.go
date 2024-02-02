package agents

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/generator"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// GenerateCmd represents the run command
var GenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "generate service template",
	Run: func(cmd *cobra.Command, args []string) {
		err := generateService(servicePath)
		cli.ExitOnError(err, "cannot generate service")
	},
}

var servicePath string

func generateService(p string) error {
	ctx, done := common.NewContext()
	defer done()
	w := wool.Get(ctx).In("agents.GenerateCmd")
	p, err := shared.SolvePath(p)
	if err != nil {
		return w.Wrapf(err, "cannot solve path")
	}

	err = generator.GenerateServiceTemplate(ctx, p)
	if err != nil {
		return w.Wrapf(err, "cannot generate service template")
	}
	return nil
}

func init() {
	GenerateCmd.PersistentFlags().StringVar(&servicePath, "service", "", "NewDir to the code to turn into library")
}
