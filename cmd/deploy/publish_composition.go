package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// `publish --all` is the deploy of a whole composition to one environment as
// one command: render what has no publishable render, publish every module
// whose delivery changes in the order the delivery checks hold them to, merge
// each promotion once the checks its base branch requires have passed, and
// report each module. It adds no mechanism of its own — each step is the
// command an operator ran by hand (render --all, publish <module>, the merge),
// and every refusal is the one that command makes.

var (
	gitOpsPublishAll   bool
	gitOpsMergeTimeout time.Duration
)

// errNothingToDeliver answers a module whose publication changes nothing on
// the delivery base and has no promotion waiting: it is not published.
var errNothingToDeliver = errors.New("nothing to deliver")

// moduleDelivery is what a whole-composition publish did with one module.
type moduleDelivery struct {
	module      string
	result      string
	pullRequest string
	commit      string
	err         error
}

// publishComposition renders what is owed, then publishes and merges every
// module of the composition for the selected environment, stopping at the
// first module that cannot be delivered: the modules after it are held to
// what it delivers.
func publishComposition(ctx context.Context) error {
	started := time.Now()
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
	composed, withdrawn := compositionRenderOrder(env, modules)
	for _, name := range withdrawn {
		cli.Info("Skipped %s: environment %s withdraws every service of it (exclude-services)", name, env.Name)
	}
	names := make([]string, 0, len(composed))
	for _, module := range composed {
		names = append(names, module.Name)
	}

	if !gitOpsYes && !models.Confirm(ctx, fmt.Sprintf("Deploy environment %s: render what is owed, then publish and merge every changed module of %s?", env.Name, strings.Join(names, ", ")), false) {
		return fmt.Errorf("deploy not confirmed")
	}

	// A render is owed only when a module has none publish would deliver;
	// then the composition renders whole, as one render derives one cell file.
	owed := map[string]error{}
	for _, name := range names {
		if why := renderOwed(ctx, workspace, env, name); why != nil {
			owed[name] = why
		}
	}
	if len(owed) > 0 {
		cli.Info("Rendering environment %s: %d of %d module(s) have no render publish would deliver", env.Name, len(owed), len(names))
		for _, name := range names {
			if why := owed[name]; why != nil {
				cli.Info("  %s: %v", name, why)
			}
		}
		if err = renderComposition(ctx); err != nil {
			return err
		}
		for _, name := range names {
			if standErr := gitops.RenderStands(ctx, workspace, env, name); standErr != nil {
				return fmt.Errorf("module %s has no publishable render even after rendering the composition: %w", name, standErr)
			}
		}
	} else {
		cli.Info("Every module of environment %s has a current render: nothing to render", env.Name)
	}

	order, err := gitops.PublishOrder(ctx, workspace, env, names)
	if err != nil {
		return err
	}
	cli.Info("Publishing %d module(s) to environment %s, in order: %s", len(order), env.Name, strings.Join(order, ", "))
	plane, err := control.NewAt(workspace.Dir())
	if err != nil {
		return err
	}
	defer plane.Close()

	deliveries := make([]moduleDelivery, 0, len(order))
	queue := slices.Clone(order)
	deferred := map[string]bool{}
	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]
		cli.Info("[%d/%d] %s", len(deliveries)+1, len(order), module)
		delivery, producer := publishAndMerge(ctx, plane, workspace.Dir(), module, queue)
		switch {
		case producer != "" && !deferred[module+"<"+producer]:
			// Its contract check refuses it against what is delivered of a
			// module this run has yet to publish: it goes right after that
			// module, once.
			deferred[module+"<"+producer] = true
			queue = slices.Insert(queue, slices.Index(queue, producer)+1, module)
			cli.Info("  deferred: its contract check is held to %s, which this run has yet to publish; publishing it after %s", producer, producer)
			continue
		case producer != "":
			// Held to the same producer again: the producers wait on each
			// other, and no order of this run delivers either.
			delivery.result, delivery.err = "failed", fmt.Errorf("its contract check is still held to %s, which this run has not published: the two are held to each other", producer)
		}
		deliveries = append(deliveries, delivery)
		if delivery.err == nil {
			continue
		}
		for _, rest := range queue {
			deliveries = append(deliveries, moduleDelivery{module: rest, result: "not reached"})
		}
		printDeliveries(env.Name, deliveries, time.Since(started))
		return fmt.Errorf("publish --all stopped at %s: %w (the modules after it were not published; run the same command again once it is fixed — the modules already delivered are unchanged and skipped)", module, delivery.err)
	}
	printDeliveries(env.Name, deliveries, time.Since(started))
	return nil
}

// renderOwed says why module has no render this deploy should publish: none
// publish would deliver (gitops.RenderStands), or one whose pin judgement no
// longer stands — a hold it relied on ended, or, without the override, pins
// it found behind (a source unreadable at render time, say).
func renderOwed(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, module string) error {
	if err := gitops.RenderStands(ctx, workspace, env, module); err != nil {
		return err
	}
	judged := gitops.RenderedPinCurrency(workspace.Dir(), env.Name, module)
	switch {
	case judged.Expired(time.Now()):
		return fmt.Errorf("a pin-hold its render relied on ended %s", judged.HeldUntil)
	case judged != nil && len(judged.Behind) > 0 && !skipWorkspaceReadiness:
		return fmt.Errorf("its render found pins behind their sources: %s", strings.Join(judged.Behind, "; "))
	}
	return nil
}

// removes reports a plan that withdraws something from the environment.
func removes(plan *gitops.PublishPlan) bool {
	return len(plan.RemovedModules)+len(plan.CellRemovals)+len(plan.UndeclaredCellRemovals)+len(plan.HostWithdrawals) > 0
}

// errHeldToAPendingProducer answers a plan whose contract check is violated
// against a producer this run still has to publish.
var errHeldToAPendingProducer = errors.New("held to a producer this run publishes later")

// publishAndMerge publishes one module and merges its promotion.
// pending are the modules this run publishes after it: a plan whose contract
// check is violated against one of them answers that producer instead of
// failing, for the run to publish it after.
func publishAndMerge(ctx context.Context, plane control.Plane, workspaceDir, module string, pending []string) (moduleDelivery, string) {
	delivery := moduleDelivery{module: module}
	fail := func(err error) (moduleDelivery, string) {
		delivery.result, delivery.err = "failed", err
		return delivery, ""
	}
	producer := ""
	if err := publishReady(ctx, "publish", workspaceDir, gitOpsEnv, module); err != nil {
		return fail(err)
	}
	request := publishRequest(module)
	result, err := control.PublishGitOps(ctx, plane, &request, func(plan *gitops.PublishPlan) error {
		// A promotion branch the publish continues has a promotion waiting
		// even when this render adds nothing to it.
		if len(plan.Changed) == 0 && plan.ExistingCommit == "" {
			return errNothingToDeliver
		}
		for _, check := range plan.ContractChecks {
			if check.Status == gitops.ContractCheckViolation && slices.Contains(pending, check.Module) {
				producer = check.Module
				return errHeldToAPendingProducer
			}
		}
		printPlanBrief(plan)
		// The one confirmation up front did not see this plan: a withdrawal
		// is confirmed when it is shown.
		if removes(plan) && !gitOpsYes && !models.Confirm(ctx, fmt.Sprintf("The promotion of %s withdraws what is listed above from %s; publish and merge it?", module, gitOpsEnv), false) {
			return fmt.Errorf("the withdrawal was not confirmed")
		}
		return nil
	})
	if errors.Is(err, errNothingToDeliver) {
		delivery.result = "unchanged"
		cli.Info("  unchanged: the delivery base already carries this render")
		return delivery, ""
	}
	if errors.Is(err, errHeldToAPendingProducer) {
		return delivery, producer
	}
	if err != nil {
		return fail(err)
	}
	delivery.pullRequest, delivery.commit = result.PullRequest, result.Commit
	cli.Info("  promotion %s", result.PullRequest)
	if result.Promoted {
		delivery.result = "promoted"
		return delivery, ""
	}
	merged, err := gitops.MergePromotion(ctx, &result, gitops.MergeOptions{
		Timeout:  gitOpsMergeTimeout,
		Progress: func(state string) { cli.Info("  %s", state) },
	})
	if err != nil {
		return fail(err)
	}
	delivery.result, delivery.commit = "merged", merged.Commit
	cli.Info("  merged #%d (%s) as %s", merged.Number, merged.Method, shortCommit(merged.Commit))
	return delivery, ""
}

// printPlanBrief is the plan as a whole-composition publish reports it: what
// it changes and every warning the plan carries, without the diff a
// single-module publish shows for review.
func printPlanBrief(plan *gitops.PublishPlan) {
	brief := *plan
	brief.Diff = ""
	changed := len(brief.Changed)
	brief.Changed = nil
	printPublishPlan(&brief)
	cli.Info("  %d changed file(s) on %s", changed, plan.PromotionBranch)
}

// printDeliveries is the run's final report: one row per module.
func printDeliveries(environment string, deliveries []moduleDelivery, elapsed time.Duration) {
	var buf bytes.Buffer
	writer := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "MODULE\tRESULT\tPROMOTION\tCOMMIT")
	for _, delivery := range deliveries {
		promotion := delivery.pullRequest
		if promotion == "" {
			promotion = "-"
		}
		commit := shortCommit(delivery.commit)
		if commit == "" {
			commit = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", delivery.module, delivery.result, promotion, commit)
	}
	_ = writer.Flush()
	cli.Info("Deploy of environment %s (%s):\n%s", environment, elapsed.Round(time.Second), strings.TrimRight(buf.String(), "\n"))
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
