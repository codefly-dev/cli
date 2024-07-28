package deploy

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/spf13/cobra"
)

// InitCmd represents the run command
var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "Init deployment",

	Run: func(cmd *cobra.Command, args []string) {
		setup()
	},
}

func setup() {
	ctx, done := common.NewContext()
	defer done()

	active, err := common.LoadActiveContext(ctx)
	cli.ExitOnError(err, "cannot load active context")

	err = deployments.SetupBaseVersionControl(ctx, active.Workspace)
	cli.ExitOnError(err, "cannot setup base version control")
}
