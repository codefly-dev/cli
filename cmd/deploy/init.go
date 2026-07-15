package deploy

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/spf13/cobra"
)

// InitCmd represents the run command
var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "Init deployment",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return setup()
	},
}

func setup() error {
	ctx, done := common.NewContext()
	defer done()
	ctx, stop := common.SignalContext(ctx)
	defer stop()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	if err := deployments.SetupBaseVersionControl(ctx, workspace); err != nil {
		return fmt.Errorf("cannot setup base version control: %w", err)
	}
	return ctx.Err()
}
