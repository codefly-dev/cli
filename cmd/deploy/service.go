package deploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the deploy command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Deploy a service to the selected workspace environment",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}
		if renderOnly {
			env, err := orchestration.SelectEnvironment(workspace, envInput)
			if err != nil {
				return err
			}
			result, err := gitops.RenderService(ctx, workspace, module, service, env, appProject, standAlone, cli.NewOutputSink())
			if err != nil {
				return fmt.Errorf("cannot render service: %w", err)
			}
			cli.Info("Rendered %s", result.Path)
			cli.Info("Digest %s", result.Inventory.Digest)
			cli.Header(1, "Service render done!")
			return nil
		}

		flow, err := initDeployService(ctx, workspace, module, service, standAlone)
		if err != nil {
			return fmt.Errorf("cannot initialize service: %w", err)
		}

		// Guarantee flow.Stop() (kills agents/containers) runs before exit.
		// cleanDeployService is guarded so it only ever runs once. The defer
		// is a panic/early-return safety net.
		var cleaned bool
		cleanup := func() error {
			if cleaned {
				return nil
			}
			cleaned = true
			return cleanDeployService(flow)
		}
		defer func() { _ = cleanup() }()

		deployErr := common.WithHeartbeat(ctx, "deploying "+service.Name, func() error {
			return deployService(ctx, flow)
		})
		stopErr := cleanup()
		var result []error
		if deployErr != nil {
			result = append(result, fmt.Errorf("service deploy failed: %w", deployErr))
		}
		if stopErr != nil {
			result = append(result, fmt.Errorf("cannot stop flow: %w", stopErr))
		}
		if len(result) > 0 {
			return errors.Join(result...)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Deployment done!")
		return nil
	},
}

func initDeployService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, standAlone bool) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("deployService", wool.ThisField(resources.WithUnique(service)))
	env, err := orchestration.SelectEnvironment(workspace, envInput)
	if err != nil {
		return nil, w.Wrap(err)
	}
	var deploymentManager deployments.Manager
	if directApplyRequested() {
		deploymentManager, err = deployments.NewLocalApplyManager(ctx, workspace, env)
		if err != nil {
			return nil, w.Wrap(err)
		}
	} else {
		deploymentManager = deployments.NewRenderManager(workspace, env)
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.DeployMode)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow.WithOutputSink(cli.NewOutputSink())
	flow.WithStandAlone(standAlone)
	err = flow.InitManagers(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot initialize managers")
	}

	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow.WithDeploymentManager(deploymentManager)
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
var renderOnly bool
var appProject string

func directApplyRequested() bool {
	return !renderOnly && !dryRun
}

func init() {
	ServiceCmd.Flags().StringVar(&envInput, "env", "local", "Environment to deploy the service")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Render the deployment without applying it")
	ServiceCmd.Flags().BoolVar(&renderOnly, "render-only", false, "Render kustomize manifests to disk without applying. Used for gitops flows where ArgoCD/Flux syncs from the rendered tree.")
	ServiceCmd.Flags().StringVar(&appProject, "app-project", "", "AppProject contract used to validate cluster-scoped rendered resources")
}
