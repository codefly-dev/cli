package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
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

	// A render is owed only when a module has none publish would deliver;
	// then the composition renders whole, as one render derives one cell file.
	owed := map[string]error{}
	for _, name := range names {
		if standErr := gitops.RenderStands(ctx, workspace, env, name); standErr != nil {
			owed[name] = standErr
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
	if !gitOpsYes && !models.Confirm(ctx, fmt.Sprintf("Publish and merge the promotion of every changed module of %s?", env.Name), false) {
		return fmt.Errorf("publication not confirmed")
	}
	plane, err := control.NewAt(workspace.Dir())
	if err != nil {
		return err
	}
	defer plane.Close()

	deliveries := make([]moduleDelivery, 0, len(order))
	for index, module := range order {
		cli.Info("[%d/%d] %s", index+1, len(order), module)
		delivery := publishAndMerge(ctx, plane, workspace.Dir(), module)
		deliveries = append(deliveries, delivery)
		if delivery.err == nil {
			continue
		}
		for _, rest := range order[index+1:] {
			deliveries = append(deliveries, moduleDelivery{module: rest, result: "not reached"})
		}
		printDeliveries(env.Name, deliveries, time.Since(started))
		return fmt.Errorf("publish --all stopped at %s: %w (the modules after it were not published; run the same command again once it is fixed — the modules already delivered are unchanged and skipped)", module, delivery.err)
	}
	printDeliveries(env.Name, deliveries, time.Since(started))
	return nil
}

// publishAndMerge publishes one module and merges its promotion.
func publishAndMerge(ctx context.Context, plane control.Plane, workspaceDir, module string) moduleDelivery {
	delivery := moduleDelivery{module: module}
	fail := func(err error) moduleDelivery {
		delivery.result, delivery.err = "failed", err
		return delivery
	}
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
		printPlanBrief(plan)
		return nil
	})
	if errors.Is(err, errNothingToDeliver) {
		delivery.result = "unchanged"
		cli.Info("  unchanged: the delivery base already carries this render")
		return delivery
	}
	if err != nil {
		return fail(err)
	}
	delivery.pullRequest, delivery.commit = result.PullRequest, result.Commit
	cli.Info("  promotion %s", result.PullRequest)
	if result.Promoted {
		delivery.result = "promoted"
		return delivery
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
	return delivery
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
	failed := 0
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
		if delivery.err != nil {
			failed++
		}
	}
	_ = writer.Flush()
	cli.Info("Deploy of environment %s (%s):\n%s", environment, elapsed.Round(time.Second), strings.TrimRight(buf.String(), "\n"))
	for _, delivery := range deliveries {
		if delivery.err != nil {
			cli.Warning("%s: %v", delivery.module, delivery.err)
		}
	}
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
