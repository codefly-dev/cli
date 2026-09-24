package deploy

import (
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

var (
	devEnv        string
	devPath       string
	devAppProject string
	devCommit     bool
	devPush       bool
)

// DevCmd is the dev escape hatch: ship one service's current code into an
// already-rendered GitOps environment without a release or a full render.
var DevCmd = &cobra.Command{
	Use:   "dev <module>/<service>",
	Short: "DEV ESCAPE HATCH: push one service's local code into an already-rendered GitOps environment",
	Long: `Build and push ONE service from local code — --path, else its machine-local
service override — through the same build path ` + "`deploy gitops render`" + ` uses, then
re-pin only that service's image digest in the environment's rendered tree.

The environment then runs code no release describes. The render inventory
records it under "dev", ` + "`codefly doctor workspace`" + ` warns while it is active, and
the next full ` + "`codefly deploy gitops render <module> --env <env>`" + ` clears it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		if devPush && !devCommit {
			return fmt.Errorf("--push needs --commit: a dev deployment is pushed only as its own commit")
		}
		target, err := resources.ParseServiceWithOptionalModule(args[0])
		if err != nil || target.Module == "" || strings.Contains(target.Module, "/") {
			return fmt.Errorf("name the service as <module>/<service>, got %q", args[0])
		}
		ctx, done := common.NewContext()
		defer done()
		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, err := loadGitOpsModule(ctx, []string{target.Module})
		if err != nil {
			return err
		}
		env, err := orchestration.SelectEnvironment(workspace, devEnv)
		if err != nil {
			return err
		}
		var override *resources.ServiceResolution
		for _, ref := range workspace.Modules {
			if ref.Name != module.Name {
				continue
			}
			resolution, resolveErr := workspace.ResolveModule(ctx, ref)
			if resolveErr != nil {
				return resolveErr
			}
			override = resolution.Services[target.Name]
		}
		source, err := gitops.ResolveDevSource(module.Name, target.Name, devPath, override)
		if err != nil {
			return err
		}
		cli.Warning("DEV DEPLOYMENT: %s/%s from %s (%s) into %s — this environment will run code no release describes until the next full render", module.Name, target.Name, source.Dir, source.Origin, env.Name)
		result, err := gitops.DeployDev(ctx, &gitops.DevRequest{
			Workspace: workspace, Module: module, Service: target.Name, Environment: env,
			AppProject: devAppProject, Source: source, Sink: cli.NewOutputSink(),
		})
		if err != nil {
			return err
		}
		cli.Info("Pinned %s", result.Entry.Image)
		for _, path := range result.Changed {
			cli.Info("  changed %s", path)
		}
		if err := gitops.StageDevDeployment(ctx, workspace.Dir(), module.Name, &result, devCommit, devPush); err != nil {
			return err
		}
		title := gitops.DevCommitTitle(module.Name, &result.Entry)
		switch {
		case devPush:
			cli.Info("Committed and pushed %q; Argo CD syncs the new digest", title)
		case devCommit:
			cli.Info("Committed %q. Push it for Argo CD to sync:\n  git -C %s push", title, workspace.Dir())
		default:
			cli.Info("Staged. Commit and push it for Argo CD to sync:\n  git -C %s commit -m %q -- %s && git -C %s push",
				workspace.Dir(), title, strings.Join(result.Changed, " "), workspace.Dir())
		}
		cli.Info("Leave dev mode with a full render: codefly deploy gitops render %s --env %s", module.Name, env.Name)
		return nil
	},
}

func init() {
	DevCmd.Flags().StringVar(&devEnv, "env", "", "Environment whose rendered tree to patch (required)")
	DevCmd.Flags().StringVar(&devPath, "path", "", "Service source directory to deploy (default: the machine-local service override)")
	DevCmd.Flags().StringVar(&devAppProject, "app-project", "", "AppProject the environment was rendered for (checked against the render)")
	DevCmd.Flags().BoolVar(&devCommit, "commit", false, "Commit the change as `dev: <module>/<service> from <path>@<sha>[-dirty]`")
	DevCmd.Flags().BoolVar(&devPush, "push", false, "Push the commit (requires --commit)")
	_ = DevCmd.MarkFlagRequired("env")
}

// printClearedDev says out loud that a full render ended dev deployments.
func printClearedDev(cleared []gitops.InventoryDevDeployment) {
	for i := range cleared {
		cli.Warning("Cleared dev deployment of %s (from %s, %s): the render re-derived its image from the workspace",
			cleared[i].Service, cleared[i].Source, cleared[i].Image)
	}
}
