package ci

import (
	"context"
	"fmt"

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
	Short: "Publish workspace state to the Codefly platform in CI",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.Init()
		defer services.ClearAgents()

		if err := platform.InitClient(ctx); err != nil {
			return fmt.Errorf("cannot initialize platform client: %w", err)
		}

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}

		if err := common.WithSilenceE(ctx, workspace, silent); err != nil {
			return fmt.Errorf("cannot configure silent services: %w", err)
		}

		if err := pushWorkspace(ctx, workspace); err != nil {
			return fmt.Errorf("cannot run CI push: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Work done!")
		return nil
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
