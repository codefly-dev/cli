package deploy

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the deploy command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Handle a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		errs := make(chan error, 1) // Buffered channel

		workspace, module, service := common.LoadRequired(ctx, args)

		flow, err := initDeployService(ctx, workspace, module, service, standAlone)
		cli.ExitOnError(err, "Cannot initialize service")

		// Guarantee flow.Stop() (kills agents/containers) runs before exit.
		// cleanDeployService is guarded so it only ever runs once. The defer
		// is a panic/early-return safety net; the normal error path captures
		// deployErr and runs cleanup explicitly BEFORE exiting, because
		// os.Exit (via cli.ExitError) does NOT run defers.
		var cleaned bool
		cleanup := func() error {
			if cleaned {
				return nil
			}
			cleaned = true
			return cleanDeployService(flow)
		}
		defer func() { _ = cleanup() }()

		go func() {
			err = common.WithHeartbeat(ctx, "deploying "+service.Name, func() error {
				return deployService(ctx, flow)
			})
			if err != nil {
				errs <- err
			}
			errs <- nil
		}()

		// deployErr captures a non-nil deploy failure so it can be reported
		// AFTER cleanup runs. Exiting here (cli.ExitOnError) would skip
		// cleanDeployService and orphan agents/containers holding ports.
		var deployErr error
	loop:
		for {
			select {
			case err := <-errs:
				deployErr = err
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		err = cleanup()
		if deployErr != nil {
			cli.ErrorChain(deployErr, "Got service deploy error")
			cli.ExitError()
		}
		cli.ExitOnError(err, "Cannot stop flow")
		if stopped != nil {
			cli.ErrorChain(stopped, "Got error while stopping service")
			cli.ExitError()
		}
		cli.Header(1, "Deployment done!")
		cli.Done()
	},
}

func initDeployService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, standAlone bool) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("deployService", wool.ThisField(resources.WithUnique(service)))
	orchestration.SetDryRun(dryRun)
	// Look up the declared env from workspace.codefly.yaml; fall back to
	// a bare-name env if the workspace hasn't migrated yet (FindEnvironment
	// already special-cases "local" → LocalEnvironment).
	env := workspace.FindEnvironment(envInput)
	if env == nil {
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

	// Apply mode (default): the LocalApplyManager runs `kustomize build
	// | kubectl apply` after each agent renders its manifests, plus
	// imports built images into k3d when the env declares it.
	//
	// Render-only mode (--render-only): skip the manager wiring. Agents
	// still write the rendered kustomize tree to disk via KustomizeDeploy
	// (in builder_deploy.go), but no kubectl apply runs. ArgoCD or a
	// separate gitops sync picks the rendered tree up from the workspace
	// once it's committed.
	if !renderOnly {
		flow.WithDeploymentManager(deployments.NewLocalApplyManager(ctx, workspace, env))
	}
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

func init() {
	ServiceCmd.Flags().StringVar(&envInput, "env", "local", "Environment to deploy the service")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Dry run the deployment")
	ServiceCmd.Flags().BoolVar(&renderOnly, "render-only", false, "Render kustomize manifests to disk without applying. Used for gitops flows where ArgoCD/Flux syncs from the rendered tree.")
}
