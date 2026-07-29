package deploy

import (
	"context"
	"fmt"
	"path"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ModuleCmd deploys every service in a module, then applies the
// module-level kustomize layer (namespace, AppProject, Applications,
// shared Ingress, etc.) at module/deployment/kustomize/overlays/<env>.
//
// Apply mode is the default. Both --render-only and --dry-run are
// no-mutation modes:
//
//	(default)  apply mode    — agents render + LocalApplyManager kubectl-applies
//	            each service, then the module-level kustomize is applied via
//	            kubectl. Used for local-k3d direct-deploy.
//
//	--render-only/--dry-run  — agents render but skip apply. Module-level
//	            kustomize is NOT applied either (it's static YAML committed
//	            to git). Used for the gitops/ArgoCD flow where ArgoCD syncs
//	            the rendered tree from git.
var ModuleCmd = &cobra.Command{
	Use:   "module [name]",
	Short: "Deploy every module service and apply its Kustomize configuration",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, err := common.LoadRequiredModuleE(ctx, args)
		if err != nil {
			return err
		}

		// Resolve env up-front: same canonical selection as deploy
		// service. Reused for every per-service flow + the final
		// module-level kustomize.
		env, err := orchestration.SelectEnvironment(workspace, envInput)
		if err != nil {
			return err
		}
		if renderOnly {
			cli.Header(2, "render-only mode — manifests written to disk, no kubectl apply")
			result, err := gitops.NewCoordinator().Render(ctx, gitops.ProduceRequest{
				Workspace: workspace, Module: module, Environment: env,
				AppProject: appProject, Sink: cli.NewOutputSink(),
			})
			if err != nil {
				return fmt.Errorf("cannot render module: %w", err)
			}
			cli.Info("Rendered %s", result.Path)
			cli.Info("Digest %s", result.Inventory.Digest)
			cli.Header(1, "Module render done!")
			return nil
		}

		var deploymentManager deployments.Manager
		var localApplyManager *deployments.LocalApplyManager
		if directApplyRequested() {
			localApplyManager, err = deployments.NewLocalApplyManager(ctx, workspace, env)
			if err != nil {
				return err
			}
			deploymentManager = localApplyManager
		} else {
			deploymentManager = deployments.NewRenderManager(workspace, env)
		}

		cli.Header(1, "Deploying module %s to env %s", module.Name, env.Name)
		if !directApplyRequested() {
			cli.Header(2, "no-mutation mode — manifests written to disk, no kubectl apply")
		}

		// Phase 1: per-service flows. Each service is its own Flow with
		// --stand-alone so the playbook only acts on that service (the
		// module loop is the dependency walker now). Failure on any
		// service aborts the module deploy — partial deploys are worse
		// than a clean failure to investigate.
		for _, ref := range module.ServiceReferences {
			cli.Header(2, "Deploying service %s", ref.Name)
			if err := deployOneService(ctx, workspace, module, ref.Name, env, deploymentManager); err != nil {
				return fmt.Errorf("cannot deploy service %s: %w", ref.Name, err)
			}
		}

		// Phase 2: module-level kustomize. Skipped in no-mutation modes
		// (the module-level YAML is static; nothing to render — it's
		// committed already). Skipped silently when the module hasn't
		// scaffolded a deployment/ folder yet.
		if directApplyRequested() {
			if err := applyModuleKustomize(ctx, module, env, localApplyManager); err != nil {
				return fmt.Errorf("cannot apply module-level kustomize: %w", err)
			}
		}

		cli.Header(1, "Module deployment done!")
		return nil
	},
}

// deployOneService runs a one-shot deploy Flow for a single service.
// Mirrors initDeployService in service.go but inline-built so the
// module loop can iterate without goroutine indirection.
func deployOneService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, name string, env *resources.Environment, deploymentManager deployments.Manager) error {
	w := wool.Get(ctx).In("deployModule.deployOneService", wool.NameField(name))

	service, err := module.LoadServiceFromName(ctx, name)
	if err != nil {
		return w.Wrapf(err, "cannot load service")
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.DeployMode)
	if err != nil {
		return w.Wrap(err)
	}
	flow.WithOutputSink(cli.NewOutputSink())
	stopped := false
	defer func() {
		if !stopped {
			_ = flow.Stop()
		}
	}()
	// Stand-alone: the module loop is the dependency walker. If we let
	// each Flow walk deps, we'd re-deploy shared services N times.
	flow.WithStandAlone(true)

	if err := flow.InitManagers(ctx); err != nil {
		return w.Wrapf(err, "cannot initialize managers")
	}
	if err := flow.Load(ctx); err != nil {
		return w.Wrap(err)
	}
	flow.WithDeploymentManager(deploymentManager)

	if err := flow.Deploy(ctx); err != nil {
		return w.Wrapf(err, "deploy failed")
	}
	err = flow.Stop()
	stopped = true
	return err
}

// applyModuleKustomize runs `kustomize build <dir> | kubectl apply -f -`
// for the module-level overlay matching the env. Silently no-ops when
// the module hasn't scaffolded module/deployment/kustomize/ yet —
// services-only modules are valid.
func applyModuleKustomize(ctx context.Context, module *resources.Module, env *resources.Environment, manager *deployments.LocalApplyManager) error {
	w := wool.Get(ctx).In("applyModuleKustomize", wool.NameField(module.Name))

	dir := path.Join(module.Dir(), "deployment", "kustomize", "overlays", env.Name)
	exists, err := shared.DirectoryExists(ctx, dir)
	if err != nil {
		return w.Wrapf(err, "cannot stat module deployment dir")
	}
	if !exists {
		w.Debug("no module-level kustomize for env, skipping", wool.DirField(dir))
		return nil
	}

	cli.Header(2, "Applying module-level kustomize at %s", dir)

	return manager.ApplyModuleKustomize(ctx, module, dir)
}

func init() {
	// Note: envInput / dryRun / renderOnly are package-level vars shared
	// with service.go's flag registration. Cobra is fine with the same
	// Go variable being the receiver for two different commands' flags
	// — each command has its own pflag set.
	ModuleCmd.Flags().StringVar(&envInput, "env", "local", "Environment to deploy the module")
	ModuleCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Render the deployment without applying it")
	ModuleCmd.Flags().BoolVar(&renderOnly, "render-only", false, "Render kustomize manifests to disk without applying. Used for gitops flows where ArgoCD/Flux syncs from the rendered tree.")
	ModuleCmd.Flags().StringVar(&appProject, "app-project", "", "AppProject contract used to validate cluster-scoped rendered resources")
}
