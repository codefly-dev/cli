package ci

import (
	"context"
	"os"
	"os/signal"

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
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		err := platform.InitClient(ctx)
		cli.ExitOnError(err, "Cannot initialize platform client")

		workspace := common.RequireWorkspace(ctx)

		err = CI(ctx, workspace, runDeployService)

		cli.ExitOnError(err, "Cannot test CI")
		cli.Header(1, "Work done!")
		cli.Exit()
	},
}

var deploymentManager *platform.DeploymentManager

func runDeployService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("deployService")
	flow, err := initDeployService(ctx, workspace, module, service)
	if err != nil {
		return w.Wrapf(err, "Cannot init flow")
	}
	err = deployService(ctx, flow, workspace)
	if err != nil {
		return w.Wrapf(err, "Cannot deploy service")
	}
	return nil
}

func initDeployService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("deployService", wool.ThisField(resources.WithUnique(service)))
	orchestration.SetDryRun(dryRun)
	var env *resources.Environment
	if envInput == "local" {
		env = resources.LocalEnvironment()
	} else {
		env = &resources.Environment{Name: envInput}
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.DeployMode)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow.WithStandAlone(standAlone)
	err = flow.InitManagers(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot initialize managers")
	}

	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	deploymentManager = platform.NewDeploymentManager(ctx, workspace, env)

	flow.WithDeploymentManager(deploymentManager)
	return flow, nil
}

func cleanDeployService(flow *orchestration.Flow) error {
	defer services.ClearAgents()
	return flow.Stop()
}

func deployService(ctx context.Context, flow *orchestration.Flow, workspace *resources.Workspace) error {
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
