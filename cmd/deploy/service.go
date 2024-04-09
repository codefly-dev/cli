package deploy

import (
	"context"
	"errors"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/builder"
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

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		errs := make(chan error, 1) // Buffered channel

		project := common.RequireProject(ctx)

		// Argument overrides directory
		service, err := common.ParseServiceArgument(ctx, project, args)
		if err != nil {
			cli.ExitOnError(err, "Cannot parse service argument")
		}
		if service == nil {
			service = common.Service(ctx)
		}

		if service == nil {
			cli.Error("No service found: use argument or run inside a service folder or use workspace")
			return
		}

		flow, err := initDeployService(ctx, project, service, standAlone)
		cli.ExitOnError(err, "Cannot initialize service")
		go func() {
			err = deployService(ctx, flow)
			if err != nil {
				errs <- err
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
	if org != "" {
		repo := "621829027644.dkr.ecr.us-east-1.amazonaws.com/kopkfeqwuk"
		builder.SetRepository(repo)
	}
	manager.SetDryRun(dryRun)

	var env *configurations.Environment
	if envInput == "local" {
		env = configurations.Local()
	} else {
		env = &configurations.Environment{Name: envInput}
	}
	if org != "" {
		env.LoadBalancer = "codefly.build"
	}

	flow, err := manager.NewFlow(ctx, project, service, env, manager.DeployMode)
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
var envInput string
var org string
var dryRun bool

func init() {
	ServiceCmd.Flags().StringVar(&org, "org", "", "Organization")
	ServiceCmd.Flags().StringVar(&envInput, "env", "local", "Environment to deploy the service")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Dry run the deployment")
}
