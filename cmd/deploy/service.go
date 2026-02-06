package deploy

import (
	"context"
	"errors"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the deploy command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Handle a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		errs := make(chan error, 1) // Buffered channel

		workspace, module, service := common.LoadRequired(ctx, args)

		flow, err := initDeployService(ctx, workspace, module, service, standAlone)
		cli.ExitOnError(err, "Cannot initialize service")

		go func() {
			err = deployService(ctx, flow)
			if err != nil {
				errs <- err
			}
			errs <- nil
		}()
		defer func(flow *orchestration.Flow) {

		}(flow)

	loop:
		for {
			select {
			case err := <-errs:
				cli.ExitOnError(err, "Got service deploy error: %v\n", err)
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		err = cleanDeployService(flow)
		cli.ExitOnError(err, "Cannot stop flow")
		if stopped != nil {
			cli.Error("Got error while stopping service: %v", errors.Unwrap(stopped))
			return
		}
		cli.Header(1, "Deployment done!")
		cli.Done()
	},
}

func initDeployService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, standAlone bool) (*orchestration.Flow, error) {
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
	// Use Local Deployment NewManager

	flow.WithDeploymentManager(deployments.NewLocalApplyManager(ctx, workspace, env))
	return flow, nil
}

func cleanDeployService(flow *orchestration.Flow) error {
	defer services.ClearAgents()
	return flow.Stop()
}

func deployService(ctx context.Context, flow *orchestration.Flow) error {
	w := wool.Get(ctx).In("deployService")
	err := flow.Deploy(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

var standAlone bool
var envInput string
var dryRun bool

func init() {
	ServiceCmd.Flags().StringVar(&envInput, "env", "local", "Environment to deploy the service")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Dry run the deployment")
}
