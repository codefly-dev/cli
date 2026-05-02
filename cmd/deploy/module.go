package deploy

import (
	"bytes"
	"context"
	"os/exec"
	"path"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/deployments"
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
// Two modes via --render-only:
//
//	(default)  apply mode    — agents render + LocalApplyManager kubectl-applies
//	            each service, then the module-level kustomize is applied via
//	            kubectl. Used for local-k3d direct-deploy.
//
//	--render-only            — agents render but skip apply. Module-level
//	            kustomize is NOT applied either (it's static YAML committed
//	            to git). Used for the gitops/ArgoCD flow where ArgoCD syncs
//	            the rendered tree from git.
var ModuleCmd = &cobra.Command{
	Use:   "module [name]",
	Short: "Deploy every service in a module + apply the module-level kustomize",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		workspace, module := loadModule(ctx, args)

		// Resolve env up-front: same workspace.FindEnvironment fallback
		// as deploy service. Reused for every per-service flow + the
		// final module-level kustomize.
		env := workspace.FindEnvironment(envInput)
		if env == nil {
			env = &resources.Environment{Name: envInput}
		}

		cli.Header(1, "Deploying module %s to env %s", module.Name, env.Name)
		if renderOnly {
			cli.Header(2, "render-only mode — manifests written to disk, no kubectl apply")
		}

		// Phase 1: per-service flows. Each service is its own Flow with
		// --stand-alone so the playbook only acts on that service (the
		// module loop is the dependency walker now). Failure on any
		// service aborts the module deploy — partial deploys are worse
		// than a clean failure to investigate.
		for _, ref := range module.ServiceReferences {
			cli.Header(2, "Deploying service %s", ref.Name)
			if err := deployOneService(ctx, workspace, module, ref.Name, env); err != nil {
				cli.ExitOnError(err, "Cannot deploy service %s", ref.Name)
			}
		}

		// Phase 2: module-level kustomize. Skipped in render-only mode
		// (the module-level YAML is static; nothing to render — it's
		// committed already). Skipped silently when the module hasn't
		// scaffolded a deployment/ folder yet.
		if !renderOnly {
			err := applyModuleKustomize(ctx, module, env)
			cli.ExitOnError(err, "Cannot apply module-level kustomize")
		}

		cli.Header(1, "Module deployment done!")
		cli.Done()
	},
}

// loadModule resolves the workspace + the named module. With no arg,
// uses the active context (active.Module). Service-less variant of
// common.LoadRequired since deploy module doesn't need a single
// service as origin.
func loadModule(ctx context.Context, args []string) (*resources.Workspace, *resources.Module) {
	if len(args) == 0 {
		workspace := common.RequireWorkspace(ctx)
		module := common.RequireModule(ctx)
		return workspace, module
	}
	workspace, err := resources.FindWorkspaceUp(ctx)
	cli.ExitOnError(err, "Cannot find workspace")
	if workspace == nil {
		cli.ExitWithMessage("No workspace found")
	}
	module, err := workspace.LoadModuleFromName(ctx, args[0])
	cli.ExitOnError(err, "Cannot load module %s", args[0])
	return workspace, module
}

// deployOneService runs a one-shot deploy Flow for a single service.
// Mirrors initDeployService in service.go but inline-built so the
// module loop can iterate without goroutine indirection.
func deployOneService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, name string, env *resources.Environment) error {
	w := wool.Get(ctx).In("deployModule.deployOneService", wool.NameField(name))

	service, err := module.LoadServiceFromName(ctx, name)
	if err != nil {
		return w.Wrapf(err, "cannot load service")
	}

	orchestration.SetDryRun(dryRun)

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.DeployMode)
	if err != nil {
		return w.Wrap(err)
	}
	// Stand-alone: the module loop is the dependency walker. If we let
	// each Flow walk deps, we'd re-deploy shared services N times.
	flow.WithStandAlone(true)

	if err := flow.InitManagers(ctx); err != nil {
		return w.Wrapf(err, "cannot initialize managers")
	}
	if err := flow.Load(ctx); err != nil {
		return w.Wrap(err)
	}
	if !renderOnly {
		flow.WithDeploymentManager(deployments.NewLocalApplyManager(ctx, workspace, env))
	}

	if err := flow.Deploy(ctx); err != nil {
		_ = flow.Stop()
		return w.Wrapf(err, "deploy failed")
	}
	return flow.Stop()
}

// applyModuleKustomize runs `kustomize build <dir> | kubectl apply -f -`
// for the module-level overlay matching the env. Silently no-ops when
// the module hasn't scaffolded module/deployment/kustomize/ yet —
// services-only modules are valid.
func applyModuleKustomize(ctx context.Context, module *resources.Module, env *resources.Environment) error {
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

	// Render with kustomize. Fails loudly if the overlay is malformed —
	// better here than mid-apply with a half-applied state.
	build := exec.CommandContext(ctx, "kustomize", "build", dir)
	var stdout, stderr bytes.Buffer
	build.Stdout = &stdout
	build.Stderr = &stderr
	if err := build.Run(); err != nil {
		return w.Wrapf(err, "kustomize build failed: %s", stderr.String())
	}

	// Resolve kubeconfig the same way the per-service path does, so a
	// single workspace.codefly.yaml env declaration drives both layers.
	configPath, err := deployments.GetK8sConfig(ctx, env)
	if err != nil {
		return w.Wrapf(err, "cannot get k8s config")
	}

	// Apply via kubectl. Splitting on `---` keeps the per-resource log
	// shape consistent with how cli/pkg/deployments/kubernetes.go
	// applies per-service manifests.
	objs := strings.Split(stdout.String(), "---")
	for _, obj := range objs {
		if strings.TrimSpace(obj) == "" {
			continue
		}
		args := []string{"--kubeconfig", configPath}
		if env.Cluster != nil && env.Cluster.Context != "" {
			args = append(args, "--context", env.Cluster.Context)
		}
		args = append(args, "apply", "-f", "-")

		apply := exec.CommandContext(ctx, "kubectl", args...)
		apply.Stdin = strings.NewReader(obj)
		var out, errOut bytes.Buffer
		apply.Stdout = &out
		apply.Stderr = &errOut
		if err := apply.Run(); err != nil {
			return w.Wrapf(err, "kubectl apply failed: %s", errOut.String())
		}
		line := strings.TrimSpace(out.String())
		if line != "" && !strings.Contains(line, "unchanged") {
			cli.Info("%s", line)
		}
	}
	return nil
}

func init() {
	// Note: envInput / dryRun / renderOnly are package-level vars shared
	// with service.go's flag registration. Cobra is fine with the same
	// Go variable being the receiver for two different commands' flags
	// — each command has its own pflag set.
	ModuleCmd.Flags().StringVar(&envInput, "env", "local", "Environment to deploy the module")
	ModuleCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Dry run the deployment")
	ModuleCmd.Flags().BoolVar(&renderOnly, "render-only", false, "Render kustomize manifests to disk without applying. Used for gitops flows where ArgoCD/Flux syncs from the rendered tree.")
}
