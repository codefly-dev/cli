package ci

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/platform"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// PushCmd represents the run command
var PushCmd = &cobra.Command{
	Use:   "push",
	Short: "Run CI Push",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		err := platform.InitClient(ctx)
		cli.ExitOnError(err, "Cannot initialize platform client")

		workspace := common.RequireWorkspace(ctx)

		common.WithSilence(ctx, workspace, silent)

		err = pushWorkspace(ctx, workspace)
		cli.ExitOnError(err, "Cannot CI push")
		cli.Header(1, "Work done!")
		cli.Exit()
	},
}

func pushWorkspace(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("pushWorkspace")
	w.Debug("pushing workspace")
	err := platform.UpdateWorkspace(ctx, workspace)
	if err != nil {
		return w.Wrapf(err, "cannot update workspace")
	}
	return nil
}

func init() {
	PushCmd.Flags().StringSliceVar(&silent, "silent", []string{}, "Silent mode")
	PushCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	PushCmd.Flags().StringVar(&scope, "scope", "", "Runtime scope (for testing encapsulation)")
	PushCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	PushCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
}
