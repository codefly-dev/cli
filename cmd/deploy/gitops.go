package deploy

import (
	"context"
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
	RunE: func(_ *cobra.Command, args []string) error {
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
		result, err := gitops.NewCoordinator().Render(ctx, gitops.ProduceRequest{
			Workspace: workspace, Module: module, Environment: env,
			AppProject: gitOpsProject, Sink: cli.NewOutputSink(),
		})
		if err != nil {
			return err
		}
		cli.Info("Rendered %s", result.Path)
		cli.Info("Digest %s", result.Inventory.Digest)
		return nil
	},
}

var gitOpsSnapshotCmd = &cobra.Command{
	Use:   "snapshot [module]",
	Short: "Render and validate the immutable service snapshot consumed by module generators",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
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
		result, err := gitops.RenderModuleSnapshot(ctx, workspace, module, env, gitOpsProject, cli.NewOutputSink())
		if err != nil {
			return err
		}
		cli.Info("Rendered service snapshot %s", result.Path)
		cli.Info("Digest %s", result.Inventory.Digest)
		return nil
	},
}

var gitOpsPlanCmd = &cobra.Command{
	Use:   "plan [module]",
	Short: "Inspect the exact GitOps publication diff",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, module, err := common.LoadRequiredModuleE(ctx, args)
		if err != nil {
			return err
		}
		request := publishRequest(module.Name)
		plan, err := gitops.NewCoordinator().PlanPublish(ctx, workspace, &request)
		if err != nil {
			return err
		}
		printPublishPlan(&plan)
		return nil
	},
}

var gitOpsPublishCmd = &cobra.Command{
	Use:   "publish [module]",
	Short: "Create a signed promotion commit and open or update its pull request",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
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
		plan, err := plane.PlanGitOpsPublish(ctx, &request)
		if err != nil {
			return err
		}
		printPublishPlan(&plan)
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
		cli.Info("Service snapshot %s", result.SnapshotRevision)
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
	RunE: func(_ *cobra.Command, args []string) error {
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
		if gitOpsRevision != "" && gitOpsRevision != publication.SnapshotRevision {
			return fmt.Errorf("requested revision %s differs from published service snapshot %s", gitOpsRevision, publication.SnapshotRevision)
		}
		plane, err := control.NewAt(workspace.Dir())
		if err != nil {
			return err
		}
		defer plane.Close()
		result, err := plane.ObserveGitOps(ctx, &gitops.ObserveRequest{
			Module: module.Name, Environment: gitOpsEnv, AppProject: gitOpsProject,
			Applications: gitOpsApplications, Revision: publication.SnapshotRevision,
			Commit: publication.Commit, Tree: publication.Tree, RenderDigest: publication.RenderDigest,
			Repository: publication.Repository, Path: publication.Path,
			PullRequest: publication.PullRequest, Local: gitOpsLocal, Timeout: gitOpsTimeout,
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
	RunE: func(_ *cobra.Command, args []string) error {
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
		plan, err := plane.PlanGitOpsRollback(ctx, &request)
		if err != nil {
			return err
		}
		printPublishPlan(&plan.PublishPlan)
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

var gitOpsRemoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Own the environment-scoped local read-only fetch remote Argo fetches from",
}

var gitOpsRemotePlanCmd = &cobra.Command{
	Use:   "plan [module]",
	Short: "Inspect the fetch remote that would serve the reviewed revision",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		remote, revision, err := loadFetchRemote(ctx, args, true)
		if err != nil {
			return err
		}
		plan := remote.Plan(revision)
		printRemotePlan(&plan)
		return nil
	},
}

var gitOpsRemoteUpCmd = &cobra.Command{
	Use:   "up [module]",
	Short: "Create or refresh the read-only fetch remote for the reviewed revision",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		remote, revision, err := loadFetchRemote(ctx, args, true)
		if err != nil {
			return err
		}
		status, err := remote.Up(ctx, revision)
		if err != nil {
			return err
		}
		cli.Info("Fetch remote %s is serving %s", status.State.Name, status.Revision)
		cli.Info("Argo repository %s", status.ArgoRepository)
		cli.Info("Repository CA %s", status.CACertPath)
		printRemoteFindings(status.Findings)
		return nil
	},
}

var gitOpsRemoteStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Validate the fetch remote against its exact ownership and network identity",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		remote, _, err := loadFetchRemote(ctx, nil, false)
		if err != nil {
			return err
		}
		status, err := remote.Status(ctx, "")
		if err != nil {
			return err
		}
		cli.Info("Fetch remote %s", status.Spec.ContainerName)
		cli.Info("Argo repository %s", status.ArgoRepository)
		if status.Revision != "" {
			cli.Info("Reviewed revision %s", status.Revision)
		}
		printRemoteFindings(status.Findings)
		return nil
	},
}

var gitOpsRemoteDownCmd = &cobra.Command{
	Use:   "down",
	Short: "Tear down the fetch remote after re-validating ownership, preserving repository data",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		remote, _, err := loadFetchRemote(ctx, nil, false)
		if err != nil {
			return err
		}
		if !gitOpsYes && !models.Confirm(ctx, "Tear down the local fetch remote (repository data is preserved)?", false) {
			return fmt.Errorf("teardown not confirmed")
		}
		if err := remote.Down(ctx); err != nil {
			return err
		}
		cli.Info("Fetch remote %s removed", remote.Spec.ContainerName)
		return nil
	},
}

func loadFetchRemote(ctx context.Context, args []string, requireRevision bool) (*gitops.FetchRemote, string, error) {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return nil, "", err
	}
	remote, err := gitops.NewFetchRemote(workspace, gitOpsEnv)
	if err != nil {
		return nil, "", err
	}
	if !requireRevision {
		return remote, "", nil
	}
	if len(args) == 0 {
		return nil, "", fmt.Errorf("a module is required to locate the reviewed revision to serve")
	}
	publication, err := gitops.LoadPublishResult(workspace.Dir(), args[0], gitOpsEnv)
	if err != nil {
		return nil, "", err
	}
	return remote, publication.SnapshotRevision, nil
}

func printRemotePlan(plan *gitops.RemotePlan) {
	cli.Info("Container %s", plan.ContainerName)
	cli.Info("Image %s", plan.Image)
	cli.Info("Network %s", plan.Network)
	cli.Info("Host verification %s", plan.HostBinding)
	cli.Info("Argo repository %s", plan.ArgoRepository)
	cli.Info("Mirror source %s", plan.SourceRepo)
	if plan.Revision != "" {
		cli.Info("Reviewed revision %s", plan.Revision)
	}
	cli.Info("Certificate validity %s", plan.CertValidity)
	for _, mount := range plan.Mounts {
		cli.Info("  mount %s", mount)
	}
}

func printRemoteFindings(findings []gitops.RemoteFinding) {
	if len(findings) == 0 {
		cli.Info("No drift detected")
		return
	}
	for _, finding := range findings {
		cli.Warning("[%s] %s", finding.Severity, finding.Message)
	}
}

func publishRequest(module string) gitops.PublishRequest {
	return gitops.PublishRequest{
		Module: module, Environment: gitOpsEnv,
		PromotionBranch: gitOpsBranch, CommitMessage: gitOpsMessage,
		Title: gitOpsTitle, Body: gitOpsBody, Local: gitOpsLocal,
	}
}

func printPublishPlan(plan *gitops.PublishPlan) {
	cli.Info("Plan %s", plan.ID)
	cli.Info("Repository %s", plan.Repository)
	cli.Info("Base %s@%s", plan.BaseBranch, plan.BaseRevision)
	cli.Info("Promotion branch %s", plan.PromotionBranch)
	cli.Info("Render digest %s", plan.RenderDigest)
	cli.Info("Service snapshot %s", plan.SnapshotRevision)
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
	GitOpsCmd.AddCommand(gitOpsSnapshotCmd, gitOpsRenderCmd, gitOpsPlanCmd, gitOpsPublishCmd, gitOpsObserveCmd, gitOpsRollbackCmd)
	for _, command := range []*cobra.Command{gitOpsSnapshotCmd, gitOpsRenderCmd, gitOpsPlanCmd, gitOpsPublishCmd, gitOpsObserveCmd, gitOpsRollbackCmd} {
		command.Flags().StringVar(&gitOpsEnv, "env", "local", "Environment to promote")
	}
	for _, command := range []*cobra.Command{gitOpsSnapshotCmd, gitOpsRenderCmd} {
		command.Flags().StringVar(&gitOpsProject, "app-project", "", "AppProject contract for cluster-scoped resources")
	}
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
	gitOpsObserveCmd.Flags().StringVar(&gitOpsRevision, "revision", "", "Expected immutable service snapshot revision")
	gitOpsObserveCmd.Flags().BoolVar(&gitOpsLocal, "local", false, "Observe a disposable local GitOps qualification")
	gitOpsObserveCmd.Flags().DurationVar(&gitOpsTimeout, "timeout", 10*time.Minute, "Maximum time to wait for Synced and Healthy")
	gitOpsRollbackCmd.Flags().StringVar(&gitOpsRollbackRevision, "to-revision", "", "Previously reviewed Git revision to re-promote")
	_ = gitOpsObserveCmd.MarkFlagRequired("app-project")
	_ = gitOpsObserveCmd.MarkFlagRequired("application")
	_ = gitOpsRollbackCmd.MarkFlagRequired("to-revision")

	GitOpsCmd.AddCommand(gitOpsRemoteCmd)
	gitOpsRemoteCmd.AddCommand(gitOpsRemotePlanCmd, gitOpsRemoteUpCmd, gitOpsRemoteStatusCmd, gitOpsRemoteDownCmd)
	for _, command := range []*cobra.Command{gitOpsRemotePlanCmd, gitOpsRemoteUpCmd, gitOpsRemoteStatusCmd, gitOpsRemoteDownCmd} {
		command.Flags().StringVar(&gitOpsEnv, "env", "local", "Environment whose fetch remote to manage")
	}
	gitOpsRemoteDownCmd.Flags().BoolVarP(&gitOpsYes, "yes", "y", false, "Tear down without an interactive confirmation")
}
