package deploy

import (
	"fmt"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/spf13/cobra"
)

var GitOpsCmd = &cobra.Command{
	Use:   "gitops",
	Short: "Render, publish, observe, and recover reviewed GitOps promotions",
}

var gitOpsRenderCmd = &cobra.Command{
	Use:   "render [module]",
	Short: "Render and validate a module-owned manifest tree",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, module, err := common.LoadRequiredModuleE(ctx, args)
		if err != nil {
			return err
		}
		env, err := orchestration.SelectEnvironment(workspace, gitOpsEnv)
		if err != nil {
			return err
		}
		result, err := gitops.RenderModule(ctx, workspace, module, env, gitOpsProject, cli.NewOutputSink())
		if err != nil {
			return err
		}
		cli.Info("Rendered %s", result.Path)
		cli.Info("Digest %s", result.Inventory.Digest)
		return nil
	},
}

var gitOpsPlanCmd = &cobra.Command{
	Use:   "plan [module]",
	Short: "Inspect the exact GitOps publication diff",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, module, err := common.LoadRequiredModuleE(ctx, args)
		if err != nil {
			return err
		}
		plan, err := gitops.PlanPublish(ctx, workspace, publishRequest(module.Name))
		if err != nil {
			return err
		}
		printPublishPlan(plan)
		return nil
	},
}

var gitOpsPublishCmd = &cobra.Command{
	Use:   "publish [module]",
	Short: "Create a signed promotion commit and open or update its pull request",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, module, err := common.LoadRequiredModuleE(ctx, args)
		if err != nil {
			return err
		}
		request := publishRequest(module.Name)
		plane, err := control.NewAt(workspace.Dir())
		if err != nil {
			return err
		}
		defer plane.Close()
		plan, err := plane.PlanGitOpsPublish(ctx, request)
		if err != nil {
			return err
		}
		printPublishPlan(plan)
		if !gitOpsYes && !models.Confirm(ctx, "Publish this signed promotion and open or update its pull request?", false) {
			return fmt.Errorf("publication not confirmed")
		}
		if err := plane.ConfigureMutationAuthority(ctx, control.AuthorityConfig{Mode: control.AuthorityPrepared}); err != nil {
			return err
		}
		prepared, err := plane.PrepareMutation(ctx, control.Mutation{
			Kind:    control.MutationGitOpsPublish,
			Summary: "Publish reviewed GitOps promotion",
			Payload: gitops.PublishMutation{Request: request, PlanID: plan.ID},
		})
		if err != nil {
			return err
		}
		mutationResult, err := plane.ApplyPreparedMutation(ctx, prepared)
		if err != nil {
			return err
		}
		if mutationResult.GitOpsPublish == nil {
			return fmt.Errorf("publication returned no result")
		}
		result := *mutationResult.GitOpsPublish
		cli.Info("Signed commit %s", result.Commit)
		cli.Info("Tree %s", result.Tree)
		cli.Info("Pull request %s", result.PullRequest)
		return nil
	},
}

var gitOpsObserveCmd = &cobra.Command{
	Use:   "observe [module]",
	Short: "Verify Argo CD reconciled the reviewed Git revision and store evidence",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, module, err := common.LoadRequiredModuleE(ctx, args)
		if err != nil {
			return err
		}
		publication, err := gitops.LoadPublishResult(workspace.Dir(), module.Name, gitOpsEnv)
		if err != nil {
			return err
		}
		plane, err := control.NewAt(workspace.Dir())
		if err != nil {
			return err
		}
		defer plane.Close()
		result, err := plane.ObserveGitOps(ctx, gitops.ObserveRequest{
			Module: module.Name, Environment: gitOpsEnv, AppProject: gitOpsProject,
			Applications: gitOpsApplications, Revision: gitOpsRevision,
			Commit: publication.Commit, Tree: publication.Tree, RenderDigest: publication.RenderDigest,
			PullRequest: publication.PullRequest, Timeout: gitOpsTimeout,
		})
		if err != nil {
			return err
		}
		cli.Info("Argo CD revision %s is Healthy", result.Evidence.ArgoRevision)
		cli.Info("Evidence %s", result.Path)
		return nil
	},
}

var gitOpsRollbackCmd = &cobra.Command{
	Use:   "rollback [module]",
	Short: "Re-promote a prior reviewed Git tree through a new pull request",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, module, err := common.LoadRequiredModuleE(ctx, args)
		if err != nil {
			return err
		}
		request := gitops.RollbackRequest{
			PublishRequest: publishRequest(module.Name),
			ToRevision:     gitOpsRollbackRevision,
		}
		plane, err := control.NewAt(workspace.Dir())
		if err != nil {
			return err
		}
		defer plane.Close()
		plan, err := plane.PlanGitOpsRollback(ctx, request)
		if err != nil {
			return err
		}
		printPublishPlan(plan.PublishPlan)
		if !gitOpsYes && !models.Confirm(ctx, "Publish this reviewed GitOps re-promotion?", false) {
			return fmt.Errorf("rollback publication not confirmed")
		}
		if err := plane.ConfigureMutationAuthority(ctx, control.AuthorityConfig{Mode: control.AuthorityPrepared}); err != nil {
			return err
		}
		prepared, err := plane.PrepareMutation(ctx, control.Mutation{
			Kind:    control.MutationGitOpsRollback,
			Summary: "Re-promote reviewed GitOps tree",
			Payload: gitops.RollbackMutation{Request: request, PlanID: plan.ID},
		})
		if err != nil {
			return err
		}
		mutationResult, err := plane.ApplyPreparedMutation(ctx, prepared)
		if err != nil {
			return err
		}
		if mutationResult.GitOpsPublish == nil {
			return fmt.Errorf("rollback returned no result")
		}
		result := *mutationResult.GitOpsPublish
		cli.Info("Signed rollback commit %s", result.Commit)
		cli.Info("Pull request %s", result.PullRequest)
		return nil
	},
}

func publishRequest(module string) gitops.PublishRequest {
	return gitops.PublishRequest{
		Module: module, Environment: gitOpsEnv,
		PromotionBranch: gitOpsBranch, CommitMessage: gitOpsMessage,
		Title: gitOpsTitle, Body: gitOpsBody, Local: gitOpsLocal,
	}
}

func printPublishPlan(plan gitops.PublishPlan) {
	cli.Info("Plan %s", plan.ID)
	cli.Info("Repository %s", plan.Repository)
	cli.Info("Base %s@%s", plan.BaseBranch, plan.BaseRevision)
	cli.Info("Promotion branch %s", plan.PromotionBranch)
	cli.Info("Render digest %s", plan.RenderDigest)
	cli.Info("Changed files:")
	for _, path := range plan.Changed {
		cli.Info("  %s", path)
	}
	if plan.Diff != "" {
		cli.Info("%s", plan.Diff)
	}
}

var (
	gitOpsEnv              string
	gitOpsProject          string
	gitOpsBranch           string
	gitOpsMessage          string
	gitOpsTitle            string
	gitOpsBody             string
	gitOpsRevision         string
	gitOpsRollbackRevision string
	gitOpsApplications     []string
	gitOpsTimeout          time.Duration
	gitOpsYes              bool
	gitOpsLocal            bool
)

func init() {
	GitOpsCmd.AddCommand(gitOpsRenderCmd, gitOpsPlanCmd, gitOpsPublishCmd, gitOpsObserveCmd, gitOpsRollbackCmd)
	for _, command := range []*cobra.Command{gitOpsRenderCmd, gitOpsPlanCmd, gitOpsPublishCmd, gitOpsObserveCmd, gitOpsRollbackCmd} {
		command.Flags().StringVar(&gitOpsEnv, "env", "local", "Environment to promote")
	}
	gitOpsRenderCmd.Flags().StringVar(&gitOpsProject, "app-project", "", "AppProject contract for cluster-scoped resources")
	for _, command := range []*cobra.Command{gitOpsPlanCmd, gitOpsPublishCmd, gitOpsRollbackCmd} {
		command.Flags().StringVar(&gitOpsBranch, "promotion-branch", "", "Promotion branch (deterministic default when empty)")
		command.Flags().BoolVar(&gitOpsLocal, "local", false, "Use a disposable local file Git remote for k3d qualification")
	}
	for _, command := range []*cobra.Command{gitOpsPublishCmd, gitOpsRollbackCmd} {
		command.Flags().StringVar(&gitOpsMessage, "message", "", "Signed commit message")
		command.Flags().StringVar(&gitOpsTitle, "title", "", "Promotion pull request title")
		command.Flags().StringVar(&gitOpsBody, "body", "", "Promotion pull request body")
		command.Flags().BoolVarP(&gitOpsYes, "yes", "y", false, "Publish the inspected plan without an interactive confirmation")
	}
	gitOpsObserveCmd.Flags().StringVar(&gitOpsProject, "app-project", "", "Selected Argo CD AppProject")
	gitOpsObserveCmd.Flags().StringSliceVar(&gitOpsApplications, "application", nil, "Argo CD application to observe (repeatable)")
	gitOpsObserveCmd.Flags().StringVar(&gitOpsRevision, "revision", "", "Exact reviewed Git revision Argo CD must reconcile")
	gitOpsObserveCmd.Flags().DurationVar(&gitOpsTimeout, "timeout", 10*time.Minute, "Maximum time to wait for Synced and Healthy")
	gitOpsRollbackCmd.Flags().StringVar(&gitOpsRollbackRevision, "to-revision", "", "Previously reviewed Git revision to re-promote")
	_ = gitOpsObserveCmd.MarkFlagRequired("app-project")
	_ = gitOpsObserveCmd.MarkFlagRequired("application")
	_ = gitOpsObserveCmd.MarkFlagRequired("revision")
	_ = gitOpsRollbackCmd.MarkFlagRequired("to-revision")
}
