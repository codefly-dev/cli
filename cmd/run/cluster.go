package run

import (
	"context"
	"fmt"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/localcluster"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	clusterEnvironment     string
	clusterAppProject      string
	clusterYes             bool
	clusterRebuild         bool
	clusterDeliveryTimeout time.Duration
)

// ClusterCmd is the local half of delivery (cli#918): the composition on the
// developer's k3d cluster, delivered to the local host the way a cell is.
var ClusterCmd = &cobra.Command{
	Use:   "cluster [module...]",
	Short: "Run the composition on the local k3d cluster through the signed delivery a cell receives",
	Long: `Run the composition on the environment's local k3d cluster, through the same
delivery a cell receives (docs/module-manifests.md, "Local runs"):

  1. Argo CD (installed if absent), the modules' namespaces, and
     the local host's trust anchor (one public-key verification policy admitting
     this machine's key when host.developers lists it, bound to this cluster's
     CA; re-written, and the host restarted, when the cluster is recreated);
  2. for each module (every composed module when none is named): build and
     push its images, render its tree with its module manifest, sign it as
     you (a developer the environment's
     host.developers lists — using this machine's delivery key), and
     promote it locally;
  3. the latest promotion served to the cluster's Argo CD, each module's Argo
     bootstrap applied, and every delivery Job awaited, with its receipts.

The environment must declare a k3d cluster (kind and context), a host,
host.developers, and host.trust_anchor (the path the host reads its trust
anchor from). Transport identity is the cell's mesh (a workload's ServiceAccount
is its principal): no SVID or peer file is issued or projected.
Stopping a run is not removal: withdrawing a binding is a
published withdrawal (the manifest with removed: true).`,
	RunE: func(_ *cobra.Command, args []string) error {
		return runCluster(args)
	},
}

func runCluster(modules []string) error {
	ctx, done := common.NewContext()
	defer done()
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}
	if err = composition.MaterializePinnedModules(ctx, workspace); err != nil {
		return err
	}
	if workspace, err = common.LoadWorkspace(ctx); err != nil {
		return fmt.Errorf("cannot reload workspace: %w", err)
	}
	env, err := orchestration.SelectEnvironment(workspace, clusterEnvironment)
	if err != nil {
		return err
	}
	if err = localcluster.Check(env); err != nil {
		return err
	}
	if err = environments.ValidateHostComposition(ctx, workspace, env); err != nil {
		return err
	}
	target, err := deployments.VerifyLocalK3dTarget(ctx, env)
	if err != nil {
		return err
	}
	if len(modules) == 0 {
		for _, reference := range workspace.Modules {
			modules = append(modules, reference.Name)
		}
	}
	if err = gitops.CheckCompositionPermissions(ctx, workspace, env, true, modules...); err != nil {
		return err
	}
	plane, err := control.NewAt(workspace.Dir())
	if err != nil {
		return err
	}
	defer plane.Close()
	driver := &localcluster.ClusterDriver{
		Workspace: workspace, Environment: env, Plane: plane,
		Kubectl: deployments.VerifiedKubectl(env, &target),
		LoadModule: func(ctx context.Context, name string) (*resources.Module, error) {
			return workspace.LoadModuleFromName(ctx, name)
		},
		Approve:      approveClusterPublish(ctx),
		Sink:         cli.NewOutputSink(),
		Rebuild:      clusterRebuild,
		AppProject:   clusterAppProject,
		AwaitTimeout: clusterDeliveryTimeout,
	}
	report, err := localcluster.Run(ctx, env, modules, driver, cli.Info)
	if err != nil {
		return err
	}
	cli.Info("%d module(s) promoted to %s at %s; %d delivery Job(s) answered", len(report.Published), env.Name, report.Revision, len(report.Deliveries))
	return nil
}

// approveClusterPublish shows what each promotion changes and delivers, and
// asks before executing it unless --yes.
func approveClusterPublish(ctx context.Context) func(plan *gitops.PublishPlan) error {
	return func(plan *gitops.PublishPlan) error {
		cli.Info("Plan %s for %s: %d changed file(s) on %s", plan.ID, plan.Module, len(plan.Changed), plan.BaseBranch)
		if plan.Delivery != nil {
			for _, document := range plan.Delivery.Documents {
				cli.Info("  %s %s at generation %d", document.Kind, document.ID, document.Generation)
			}
		}
		if clusterYes || models.Confirm(ctx, "Sign and promote "+plan.Module+" to the local cluster?", false) {
			return nil
		}
		return fmt.Errorf("promotion of %s not confirmed", plan.Module)
	}
}

func init() {
	ClusterCmd.Flags().StringVar(&clusterEnvironment, "env", orchestration.LocalEnvironmentName, "k3d environment to run on (declares cluster.kind k3d, cluster.context, host, host.developers and host.trust_anchor)")
	ClusterCmd.Flags().StringVar(&clusterAppProject, "app-project", "", "AppProject contract each module's Argo bootstrap is stamped with")
	ClusterCmd.Flags().BoolVarP(&clusterYes, "yes", "y", false, "Sign and promote every module without an interactive confirmation")
	ClusterCmd.Flags().BoolVar(&clusterRebuild, "rebuild", false, "Build every image even when no input of it changed")
	ClusterCmd.Flags().DurationVar(&clusterDeliveryTimeout, "delivery-timeout", 30*time.Minute, "Maximum time to wait for each delivery Job (the Job itself gives up after 25 minutes)")
}
