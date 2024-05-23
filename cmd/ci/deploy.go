package ci

import (
	"context"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// DeployCmd represents the run command
var DeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Run CI Deploy",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		workspace := common.RequireWorkspace(ctx)

		common.WithSilence(ctx, workspace, silent)

		err := CI(ctx, workspace, runDeployService)

		cli.ExitOnError(err, "Cannot test CI")
		cli.Header(1, "Work done!")
		cli.Exit()
	},
}

func runDeployService(ctx context.Context, workspace *resources.Workspace, service *resources.Service) error {
	w := wool.Get(ctx).In("deployService")
	flow, err := initDeployService(ctx, workspace, service)
	if err != nil {
		return w.Wrapf(err, "Cannot init flow")
	}
	err = deployService(ctx, flow)
	if err != nil {
		return w.Wrapf(err, "Cannot test service")
	}
	return nil
}

func initDeployService(ctx context.Context, workspace *resources.Workspace, service *resources.Service) (*manager.Flow, error) {
	w := wool.Get(ctx).In("TestService", wool.ThisField(service))
	// Catch panic
	defer w.Catch()

	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}

	flow, err := manager.NewFlow(ctx, workspace, service, resources.LocalEnvironment(), manager.DeployMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithLoadOnly(loadOnly)
	flow.WithInitOnly(initOnly)
	flow.WithStandAlone(true)
	flow.WithRuntimeContext(runtimeContext)

	err = flow.InitManagers(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return flow, nil
}

func deployService(ctx context.Context, flow *manager.Flow) error {
	// Catch panic
	w := wool.Get(ctx).In("TestService")
	defer w.Catch()
	err := flow.Start(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil
}

func init() {
	DeployCmd.Flags().StringSliceVar(&silent, "silent", []string{}, "Silent mode")
	DeployCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	DeployCmd.Flags().StringVar(&scope, "scope", "", "Runtime scope (for testing encapsulation)")
	DeployCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	DeployCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
}
