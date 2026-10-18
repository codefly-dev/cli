package deploy

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// loadGitOpsModule is how every gitops verb finds the module it acts on. It
// materializes the workspace's composed pinned modules first: a CI render
// always starts from a fresh checkout with no codefly.local.yaml, where the
// module named on the command line is exactly what core refuses to load until
// the CLI has pulled it — the same materialization `codefly run` performs, with
// the same cache, receipts and overlay, and free once it has happened.
var loadGitOpsModule = common.LoadRequiredModuleWithPinnedModulesE

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
		if gitOpsRenderAll {
			if len(args) > 0 {
				return fmt.Errorf("--all renders every composed module; name no module with it")
			}
			return renderComposition(ctx)
		}
		workspace, module, err := loadGitOpsModule(ctx, args)
		if err != nil {
			return err
		}
		if err = requireWorkspaceReady(ctx, "render", gitOpsEnv, module.Name); err != nil {
			return err
		}
		env, err := orchestration.SelectEnvironment(workspace, gitOpsEnv)
		if err != nil {
			return err
		}
		// The module asked for, then the modules that read a workspace
		// configuration group it found changed since they rendered it: publish
		// refuses until every reader bakes in the same value, and nothing else
		// needs to move. The cell file is derived once, after the last.
		var composed []*resources.Module
		render := func(ctx context.Context, name string) (gitops.RenderResult, error) {
			target := module
			if name != module.Name {
				if composed == nil {
					loaded, loadErr := workspace.LoadModules(ctx)
					if loadErr != nil {
						return gitops.RenderResult{}, loadErr
					}
					composed = loaded
				}
				index := slices.IndexFunc(composed, func(candidate *resources.Module) bool { return candidate.Name == name })
				if index < 0 {
					return gitops.RenderResult{}, fmt.Errorf("the workspace composes no module %s", name)
				}
				target = composed[index]
				if readyErr := requireWorkspaceReady(ctx, "render", gitOpsEnv, name); readyErr != nil {
					return gitops.RenderResult{}, readyErr
				}
			}
			result, renderErr := gitops.NewCoordinator().Render(ctx, gitops.ProduceRequest{
				Workspace: workspace, Module: target, Environment: env,
				AppProject: gitOpsProject, ValidateCluster: gitOpsValidateCluster,
				Rebuild: gitOpsRebuild, WithoutCell: true, Sink: cli.NewOutputSink(),
			})
			if renderErr != nil {
				return result, renderErr
			}
			cli.Info("Rendered %s", result.Path)
			cli.Info("Digest %s", result.Inventory.Digest)
			printSizingReport(result.Sizing)
			printElidedNamespaces(result.ElidedNamespaces)
			// Its stale readers render next in this run, so they are not
			// reported as owed.
			printed := result
			printed.StaleGroupConsumers = nil
			printModuleManifest(&printed)
			printClearedDev(result.ClearedDev)
			return result, nil
		}
		if _, err = gitops.RenderGroupReaders(ctx, module.Name, render, func(reader string, groups []string) {
			cli.Info("Rendering %s: it reads workspace configuration group %s, which changed since it rendered", reader, strings.Join(groups, ", "))
		}); err != nil {
			return err
		}
		if env.Host != nil {
			cell, cellErr := gitops.RenderCell(ctx, workspace, env)
			if cellErr != nil {
				cli.Warning("Cell file not re-derived: %s", cellErr)
				return nil
			}
			printCell(&cell)
		}
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
		workspace, module, err := loadGitOpsModule(ctx, args)
		if err != nil {
			return err
		}
		if err = requireWorkspaceReady(ctx, "snapshot", gitOpsEnv, module.Name); err != nil {
			return err
		}
		env, err := orchestration.SelectEnvironment(workspace, gitOpsEnv)
		if err != nil {
			return err
		}
		result, err := gitops.NewCoordinator().Render(ctx, gitops.ProduceRequest{
			Workspace: workspace, Module: module, Environment: env,
			AppProject: gitOpsProject, Snapshot: true,
			Rebuild: gitOpsRebuild, Sink: cli.NewOutputSink(),
		})
		if err != nil {
			return err
		}
		cli.Info("Rendered service snapshot %s", result.Path)
		cli.Info("Digest %s", result.Inventory.Digest)
		printSizingReport(result.Sizing)
		printElidedNamespaces(result.ElidedNamespaces)
		printModuleManifest(&result)
		printClearedDev(result.ClearedDev)
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
		workspace, module, err := loadGitOpsModule(ctx, args)
		if err != nil {
			return err
		}
		if err = requireWorkspaceReady(ctx, "plan", gitOpsEnv, module.Name); err != nil {
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
		workspace, module, err := loadGitOpsModule(ctx, args)
		if err != nil {
			return err
		}
		if err = requireWorkspaceReady(ctx, "publish", gitOpsEnv, module.Name); err != nil {
			return err
		}
		request := publishRequest(module.Name)
		plane, err := control.NewAt(workspace.Dir())
		if err != nil {
			return err
		}
		defer plane.Close()
		result, err := control.PublishGitOps(ctx, plane, &request, func(plan *gitops.PublishPlan) error {
			printPublishPlan(plan)
			if !gitOpsYes && !models.Confirm(ctx, "Publish this signed promotion and open or update its pull request?", false) {
				return fmt.Errorf("publication not confirmed")
			}
			return nil
		})
		if err != nil {
			return err
		}
		cli.Info("Service snapshot %s", result.SnapshotRevision)
		cli.Info("Signed commit %s", result.Commit)
		cli.Info("Tree %s", result.Tree)
		printDelivery(result.Delivery)
		cli.Info("Pull request %s", result.PullRequest)
		if result.Promoted {
			cli.Info("Promoted locally: %s now points at %s (the developer's signed publish is the approval on a local cluster)", result.BaseBranch, result.Commit)
		}
		return nil
	},
}

// printDelivery reports the delivery documents a publish settled and signed,
// and says plainly when they are unsigned: a host refuses those.
func printDelivery(delivery *gitops.InventoryDelivery) {
	if delivery == nil {
		return
	}
	for _, document := range delivery.Documents {
		state := "settled"
		if document.Removed {
			state = "withdrawn by a tombstone"
		}
		cli.Info("Delivered %s document %s at generation %d, %s", document.Kind, document.ID, document.Generation, state)
	}
	if delivery.Signed {
		cli.Info("Delivery documents signed by %s", delivery.Identity)
		return
	}
	cli.Warning("Delivery documents are UNSIGNED: no signing identity in this process; a host refuses them, and no Job delivers them. Publish from the release workflow — or, to a host on a local cluster, list your identity in the k3d environment's host.developers and publish --local to sign as yourself.")
}

var gitOpsObserveCmd = &cobra.Command{
	Use:   "observe [module]",
	Short: "Verify Argo CD reconciled the reviewed Git revision and store evidence",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, module, err := loadGitOpsModule(ctx, args)
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
		workspace, module, err := loadGitOpsModule(ctx, args)
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
		if err = plane.ConfigureMutationAuthority(ctx, control.AuthorityConfig{Mode: control.AuthorityPrepared}); err != nil {
			return err
		}
		prepared, err := plane.PrepareMutation(ctx, control.Mutation{
			Kind:    control.MutationGitOpsRollback,
			Summary: "Re-promote reviewed GitOps tree",
			Payload: gitops.RollbackMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers},
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
	Short: "Create or refresh the read-only fetch remote for the reviewed revision (no module: the latest local promotion)",
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
		// With no module, the environment's latest LOCAL PROMOTION: what a
		// developer's signed publish promoted on a k3d environment whose host
		// lists developers (the base branch of the local delivery repository).
		revision, promotionErr := gitops.LatestLocalPromotion(ctx, workspace, gitOpsEnv)
		if promotionErr != nil {
			return nil, "", fmt.Errorf("name a module to serve its reviewed revision, or promote one locally first: %w", promotionErr)
		}
		return remote, revision, nil
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
		AllowUnresolvedContracts: gitOpsAllowUnresolvedContracts,
		Resign:                   gitOpsResign,
	}
}

// printSizingReport surfaces the render-time resource sizing: the reservation
// totals a target cell must schedule (per-replica amounts multiplied by replica
// count), the per-replica sizing of each workload, and a warning for any
// workload with a container missing CPU or memory requests or limits.
// printElidedNamespaces reports the Namespace manifests the render dropped
// because they claimed the externally provisioned destination namespace. The
// render is correct either way, but the service agent that emitted one is not,
// so the operator is told which one to fix rather than left with a quietly
// different committed tree.
func printElidedNamespaces(elided []string) {
	if len(elided) == 0 {
		return
	}
	cli.Warning("%d Namespace manifest(s) claiming the destination namespace were dropped: the namespace is provisioned outside this render (Argo Applications carry CreateNamespace=false)", len(elided))
	for _, path := range elided {
		cli.Warning("  %s — its service agent should elide the Namespace under a restricted output profile", path)
	}
}

// printModuleManifest reports what this render DECLARED should be present on
// the host, and why it declared nothing when the reason is the module's own.
func printModuleManifest(result *gitops.RenderResult) {
	if declared := result.ModuleManifest; declared != nil {
		cli.Info("Declared module manifest %s at provisional generation %d (%s); publish settles the generation against the delivery repository and signs the manifest", declared.Binding, declared.Generation, declared.Path)
	} else if result.UndeclaredManifest != "" {
		cli.Info("No module manifest: %s", result.UndeclaredManifest)
	}
	groups := make([]string, 0, len(result.StaleGroupConsumers))
	for group := range result.StaleGroupConsumers {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		cli.Warning("Workspace configuration group %s changed since %s rendered it; render those modules next — publish refuses this one until they bake in the same value", group, strings.Join(result.StaleGroupConsumers[group], ", "))
	}
}

func printSizingReport(report gitops.SizingReport) {
	if len(report.Workloads) == 0 {
		return
	}
	cli.Info("Sizing %d workloads — total reserved: requests %s CPU / %s memory, limits %s CPU / %s memory",
		len(report.Workloads),
		report.TotalRequests.CPUString(), report.TotalRequests.MemoryString(),
		report.TotalLimits.CPUString(), report.TotalLimits.MemoryString())
	for _, workload := range report.Workloads {
		cli.Info("  %s/%s x%d — %s CPU / %s memory requested per replica",
			workload.Kind, workload.Name, workload.Replicas,
			workload.Requests.CPUString(), workload.Requests.MemoryString())
	}
	if report.WorkloadsMissingRequests > 0 {
		cli.Warning("%d workload(s) have a container missing CPU or memory requests", report.WorkloadsMissingRequests)
	}
	if report.WorkloadsMissingLimits > 0 {
		cli.Warning("%d workload(s) have a container missing CPU or memory limits", report.WorkloadsMissingLimits)
	}
}

// formatContractChecks renders the plan's contract admission checks as an
// aligned table (STATUS, UNIT, the module/service/endpoint being consumed,
// and MESSAGE), matching the "prints them as a table" behavior documented in
// docs/commands.md rather than a flat bulleted list.
func formatContractChecks(checks []gitops.ContractCheck) string {
	var buf bytes.Buffer
	writer := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "STATUS\tUNIT\tCONTRACT\tMESSAGE")
	for _, check := range checks {
		fmt.Fprintf(writer, "%s\t%s\t%s/%s/%s\t%s\n", check.Status, check.Unit, check.Module, check.Service, check.Endpoint, check.Message)
	}
	_ = writer.Flush()
	return strings.TrimRight(buf.String(), "\n")
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
	if len(plan.ContractChecks) > 0 {
		cli.Info("Contract checks:")
		cli.Info("%s", formatContractChecks(plan.ContractChecks))
	}
	for _, stale := range plan.StaleConsumers {
		cli.Warning("Delivered consumer %s bakes in another value of a workspace configuration group this publication carries; publish it next, so %s does not keep delivering two values of one group", stale, plan.Environment)
	}
	if plan.Diff != "" {
		cli.Info("%s", plan.Diff)
	}
}

var (
	gitOpsEnv                      string
	gitOpsProject                  string
	gitOpsBranch                   string
	gitOpsMessage                  string
	gitOpsTitle                    string
	gitOpsBody                     string
	gitOpsRevision                 string
	gitOpsRollbackRevision         string
	gitOpsApplications             []string
	gitOpsTimeout                  time.Duration
	gitOpsYes                      bool
	gitOpsLocal                    bool
	gitOpsAllowUnresolvedContracts bool
	gitOpsResign                   bool
	gitOpsValidateCluster          bool
	gitOpsRebuild                  bool
)

func init() {
	GitOpsCmd.AddCommand(gitOpsSnapshotCmd, gitOpsRenderCmd, gitOpsPlanCmd, gitOpsPublishCmd, gitOpsObserveCmd, gitOpsRollbackCmd)
	for _, command := range []*cobra.Command{gitOpsSnapshotCmd, gitOpsRenderCmd, gitOpsPlanCmd, gitOpsPublishCmd, gitOpsObserveCmd, gitOpsRollbackCmd} {
		command.Flags().StringVar(&gitOpsEnv, "env", "local", "Environment to promote")
	}
	// The delivery verbs — the ones that render a manifest tree from the
	// workspace's environment configuration, or promote one — evaluate the
	// `doctor workspace` verdict first. `observe`, `rollback` and the `remote`
	// verbs deliberately do not: they act on an already-reviewed revision and
	// are exactly what an operator reaches for while the workspace is broken.
	for _, command := range []*cobra.Command{gitOpsSnapshotCmd, gitOpsRenderCmd, gitOpsPlanCmd, gitOpsPublishCmd} {
		command.Flags().BoolVar(&skipWorkspaceReadiness, SkipWorkspaceReadinessFlag, false,
			"Proceed even when `codefly doctor workspace --env <env> --module <module>` says the workspace is not ready (for an operator mid-repair; the skip is announced in the output)")
	}
	gitOpsRenderCmd.Flags().BoolVar(&gitOpsRenderAll, "all", false,
		"Render every module the workspace composes, the host first, and derive the environment's cell file once at the end")
	gitOpsRenderCmd.Flags().BoolVar(&gitOpsValidateCluster, "validate-cluster", false,
		"Also dry-run each service's manifests server-side against the environment's declared cluster.context (off: a render needs no cluster)")
	for _, command := range []*cobra.Command{gitOpsSnapshotCmd, gitOpsRenderCmd} {
		command.Flags().StringVar(&gitOpsProject, "app-project", "", "AppProject contract for cluster-scoped resources")
		command.Flags().BoolVar(&gitOpsRebuild, "rebuild", false,
			"Build every image even when no input of it changed, instead of keeping the image already built from those inputs")
	}
	for _, command := range []*cobra.Command{gitOpsPlanCmd, gitOpsPublishCmd, gitOpsRollbackCmd} {
		command.Flags().StringVar(&gitOpsBranch, "promotion-branch", "", "Promotion branch (deterministic default when empty)")
		command.Flags().BoolVar(&gitOpsLocal, "local", false, "Use a disposable local file Git remote for k3d qualification")
	}
	for _, command := range []*cobra.Command{gitOpsPlanCmd, gitOpsPublishCmd, gitOpsRollbackCmd} {
		command.Flags().BoolVar(&gitOpsAllowUnresolvedContracts, "allow-unresolved-contracts", false,
			"Downgrade a consumed contract whose exposing module is not yet deployed to a warning, for bootstrap ordering")
		command.Flags().BoolVar(&gitOpsResign, "resign", false,
			"Sign again a delivered document whose carrier the release policy no longer admits (a rotated signing identity); deliberate, never implied")
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

// printCell reports the cell file a hosted render re-derived, what it covers
// and what it could not name.
func printCell(cell *gitops.CellResult) {
	if cell == nil {
		return
	}
	cli.Info("Cell file %s covers modules %s", cell.Path, strings.Join(cell.Modules, ", "))
	for _, edge := range cell.ConfiguredEdges {
		cli.Info("%s", edge)
	}
	if len(cell.Skipped) > 0 {
		cli.Info("Cell file leaves out %s: rendered for another environment", strings.Join(cell.Skipped, ", "))
	}
	if !cell.Source {
		cli.Info("Cell file names no source commit: the environment declares no gitops repository and branch, or the workspace is not a git checkout")
	}
}

var gitOpsRenderAll bool

// renderComposition renders every module the workspace composes for the
// selected environment — the host module first, since every other module's
// tree is read against it, then the rest in name order — and derives the
// environment's cell file once, from all of them. One command for one
// composition: a stale sibling is never a reason to run it again.
func renderComposition(ctx context.Context) error {
	if err := common.ResolvePinnedModules(ctx); err != nil {
		return err
	}
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return err
	}
	env, err := orchestration.SelectEnvironment(workspace, gitOpsEnv)
	if err != nil {
		return err
	}
	modules, err := workspace.LoadModules(ctx)
	if err != nil {
		return err
	}
	host := ""
	if env.Host != nil {
		host, _, _ = env.Host.DeliveryEndpoint()
	}
	sort.SliceStable(modules, func(i, j int) bool {
		if (modules[i].Name == host) != (modules[j].Name == host) {
			return modules[i].Name == host
		}
		return modules[i].Name < modules[j].Name
	})
	for _, module := range modules {
		if err = requireWorkspaceReady(ctx, "render", gitOpsEnv, module.Name); err != nil {
			return err
		}
		result, renderErr := gitops.NewCoordinator().Render(ctx, gitops.ProduceRequest{
			Workspace: workspace, Module: module, Environment: env,
			AppProject: gitOpsProject, ValidateCluster: gitOpsValidateCluster,
			Rebuild: gitOpsRebuild, WithoutCell: true, Sink: cli.NewOutputSink(),
		})
		if renderErr != nil {
			return fmt.Errorf("render %s: %w", module.Name, renderErr)
		}
		cli.Info("Rendered %s", result.Path)
		cli.Info("Digest %s", result.Inventory.Digest)
		printSizingReport(result.Sizing)
		printElidedNamespaces(result.ElidedNamespaces)
		printModuleManifest(&result)
		printClearedDev(result.ClearedDev)
	}
	if env.Host == nil {
		return nil
	}
	cell, err := gitops.RenderCell(ctx, workspace, env)
	if err != nil {
		return fmt.Errorf("render the cell file of environment %s: %w", env.Name, err)
	}
	printCell(&cell)
	return nil
}
