package ci

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/platform"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// DeployCmd represents the run command
var DeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Run CI Handle",
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

		if err := CI(ctx, workspace, runDeployService); err != nil {
			return fmt.Errorf("cannot run CI deploy: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Work done!")
		return nil
	},
}

func runDeployService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("deployService")
	flow, deploymentManager, err := initDeployService(ctx, workspace, module, service)
	if err != nil {
		return w.Wrapf(err, "Cannot init flow")
	}
	return runAndStopFlow(flow, func() error {
		if err := deployService(ctx, flow, deploymentManager, workspace); err != nil {
			return w.Wrapf(err, "Cannot deploy service")
		}
		return nil
	})
}

func initDeployService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, *platform.DeploymentManager, error) {
	w := wool.Get(ctx).In("deployService", wool.ThisField(resources.WithUnique(service)))
	orchestration.SetDryRun(dryRun)
	env, err := orchestration.SelectEnvironment(workspace, envInput)
	if err != nil {
		return nil, nil, w.Wrap(err)
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.DeployMode)
	if err != nil {
		return nil, nil, w.Wrap(err)
	}

	flow.WithStandAlone(standAlone)
	err = flow.InitManagers(ctx)
	if err != nil {
		return nil, nil, stopFlowAfterError(flow, w.Wrapf(err, "cannot initialize managers"))
	}

	err = flow.Load(ctx)
	if err != nil {
		return nil, nil, stopFlowAfterError(flow, w.Wrap(err))
	}
	deploymentManager := platform.NewDeploymentManager(ctx, workspace, env)

	flow.WithDeploymentManager(deploymentManager)
	return flow, deploymentManager, nil
}

func deployService(ctx context.Context, flow *orchestration.Flow, deploymentManager *platform.DeploymentManager, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("deployService")
	err := flow.Deploy(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	err = deploymentManager.Deploy(ctx, workspace)
	if err != nil {
		return w.Wrapf(err, "cannot deploy service")
	}
	return nil

}

var standAlone bool
var envInput string
var dryRun bool

func init() {
	DeployCmd.Flags().StringVar(&envInput, "env", "local", "Environment to deploy the service")
	DeployCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	DeployCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Dry run the deployment")
}
