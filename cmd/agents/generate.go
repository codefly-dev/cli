package agents

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/agents/generator"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// GenerateCmd represents the run command
var GenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "generate service template",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		return generateService(ctx, servicePath)
	},
}

var servicePath string

func generateService(ctx context.Context, p string) error {
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
