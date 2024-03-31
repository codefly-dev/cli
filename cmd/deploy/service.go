package deploy

import (
	"context"
	"errors"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the deploy command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Deploy a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		errs := make(chan error, 1) // Buffered channel

		project := common.Project(ctx)
		if project == nil {
			cli.Error("Cannot find project")
			cli.ExitError()
		}
		service := common.Service(ctx)
		if service == nil {
			cli.Error("Cannot find service")
			cli.ExitError()
		}

		flow, err := initDeployService(ctx, project, service, standAlone)
		cli.ExitOnError(err, "Cannot initialize service")
		go func() {
			err = deployService(ctx, flow)
			if err != nil {
				errs <- err
			}
			if apply {
				err = flow.Push(ctx)
				if err != nil {
					errs <- err
				}
			}
			errs <- nil
		}()
		defer func(flow *manager.Flow) {

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

func initDeployService(ctx context.Context, project *configurations.Project, service *configurations.Service, standAlone bool) (*manager.Flow, error) {
	w := wool.Get(ctx).In("deployService", wool.ThisField(service))
	var environ *configurations.Environment
	if env == "local" {
		environ = configurations.Local()
	} else {
		environ = &configurations.Environment{Name: env}
	}
	flow, err := manager.NewFlow(ctx, project, service, environ, manager.DeployMode)
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
	return flow, nil
}

func cleanDeployService(flow *manager.Flow) error {
	defer services.ClearAgents()
	return flow.Stop()
}

func deployService(ctx context.Context, flow *manager.Flow) error {
	w := wool.Get(ctx).In("deployService")
	err := flow.Deploy(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

var standAlone bool
var apply bool
var env string

func init() {
	ServiceCmd.Flags().StringVar(&env, "env", "local", "Environment to deploy the service")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&apply, "apply", false, "Apply the deployment")
}
