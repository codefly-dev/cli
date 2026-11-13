package gitops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/google/go-github/v89/github"
)

// Merging a promotion is the last step of a publication, and the one that
// used to be done by polling the pull request's merge state until it read
// CLEAN. A delivery repository carries checks that are not required — a
// workflow about another environment, failing for reasons of its own — and
// they keep the state UNSTABLE forever, so the poll timed out on promotions
// the repository would have merged. What the base branch requires is the
// whole condition: its required status checks (branch protection and
// rulesets alike) passing on the promotion's head. The merge itself — the
// mergeable state, a strict base caught up, the merge queue, a merge
// confirmed after a transport error — is the CLI's one merge loop
// (gh.PullRequestMerge), which a release lands its bump through too; only
// which checks count, and how the merge is made, are a promotion's own.

// MergeOptions bounds and narrates a promotion's merge.
type MergeOptions struct {
	// Timeout bounds the wait for the required checks and the queue.
	Timeout time.Duration
	// Interval is how often the pull request is read again (default 15s).
	Interval time.Duration
	// Progress is told each new state of what the merge waits on.
	Progress func(string)
}

// MergeResult is a merged promotion.
type MergeResult struct {
	Number int
	// Commit is the commit the base branch carries for the promotion.
	Commit string
	// Method is how it was asked to merge: merge, squash or rebase.
	Method string
	// Required are the checks the base branch required, by name.
	Required []string
}

// requiredCheck is one status check the base branch requires: its name, and
// the app that must report it when the requirement names one.
type requiredCheck struct {
	context string
	appID   int64
}

// baseRequirements is what the base branch requires of a merge.
type baseRequirements struct {
	checks []requiredCheck
	// linear: the branch requires a linear history (no merge commit).
	linear bool
	// methods are the merge methods a ruleset allows; empty allows any.
	methods []string
}

// MergePromotion waits until every status check the promotion's base branch
// requires has passed on the promotion's head, then merges that head —
// through the branch's merge queue when it has one — and answers the commit
// the base carries. A required check that fails, a merge the repository
// refuses (a review it requires, a head that moved) and a wait past the
// timeout are errors naming their reason; a promotion already merged answers
// its merge.
func MergePromotion(ctx context.Context, result *PublishResult, options MergeOptions) (MergeResult, error) {
	merged := MergeResult{Number: result.PullRequestID}
	if result.PullRequestID == 0 {
		return merged, fmt.Errorf("the promotion of %s is not a GitHub pull request (%s), so there is nothing to merge here", result.Path, result.PullRequest)
	}
	slug, err := validateRepositoryURL(result.Repository, false)
	if err != nil {
		return merged, err
	}
	owner, repo, err := splitRepositorySlug(slug)
	if err != nil {
		return merged, err
	}
	client, err := gh.NewClient()
	if err != nil {
		return merged, err
	}
	if options.Interval <= 0 {
		options.Interval = 15 * time.Second
	}
	say := narrator(options.Progress)
	if options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.Timeout)
		defer cancel()
	}
	checks := &requiredChecks{client: client, owner: owner, repo: repo, number: result.PullRequestID, say: say}
	requires, err := checks.requirements(ctx, result.BaseBranch)
	if err != nil {
		return merged, err
	}
	checks.required = requires.checks
	for _, check := range requires.checks {
		merged.Required = append(merged.Required, check.context)
	}
	if merged.Method, err = mergeMethod(ctx, client, owner, repo, requires); err != nil {
		return merged, err
	}
	merge := &gh.PullRequestMerge{
		Client: client, Owner: owner, Repo: repo, Number: result.PullRequestID,
		Noun: "promotion pull request", Poll: options.Interval, Ready: checks.ready,
		Method: merged.Method, Head: result.Commit, MergeBlocked: true, Say: say,
	}
	merged.Commit, err = merge.Land(ctx)
	// The wait can run out between reads or inside one; either way what it
	// was waiting on is the reason.
	var open *gh.StillOpenError
	if errors.As(err, &open) || errors.Is(err, context.DeadlineExceeded) {
		waiting := checks.waiting
		if waiting == "" {
			waiting = "the merge"
		}
		return merged, fmt.Errorf("promotion pull request #%d was not merged in time: %s; it is open: %w", result.PullRequestID, waiting, err)
	}
	return merged, err
}

// narrator says a state once, however many reads repeat it.
func narrator(progress func(string)) func(string) {
	last := ""
	return func(state string) {
		if progress != nil && state != last {
			last = state
			progress(state)
		}
	}
}

// requiredChecks is the promotion's check policy: the base branch's required
// checks, read on the head the merge loop is about to land.
type requiredChecks struct {
	client      *github.Client
	owner, repo string
	number      int
	required    []requiredCheck
	say         func(string)
	// waiting names what the last read was still waiting on.
	waiting string
}

// requirements reads what the base branch requires of a merge: its required
// status checks (branch protection and rulesets), whether it requires a
// linear history, and the merge methods a ruleset allows. A GitHub that
// serves no rules endpoint (an older Enterprise answers 404) has no rules.
func (r *requiredChecks) requirements(ctx context.Context, base string) (baseRequirements, error) {
	var requires baseRequirements
	add := func(context string, appID int64) {
		if !slices.ContainsFunc(requires.checks, func(check requiredCheck) bool { return check.context == context }) {
			requires.checks = append(requires.checks, requiredCheck{context: context, appID: appID})
		}
	}
	branch, _, err := r.client.Repositories.GetBranch(ctx, r.owner, r.repo, base, 0)
	if err != nil {
		return requires, fmt.Errorf("read the protection of %s/%s's %s: %w", r.owner, r.repo, base, err)
	}
	if checks := branch.GetProtection().GetRequiredStatusChecks(); checks != nil {
		if checks.Checks != nil {
			for _, check := range *checks.Checks {
				add(check.Context, check.GetAppID())
			}
		}
		if checks.Contexts != nil {
			for _, context := range *checks.Contexts {
				add(context, 0)
			}
		}
	}
	rules, response, err := r.client.Repositories.ListRulesForBranch(ctx, r.owner, r.repo, base, nil)
	if err != nil && (response == nil || response.StatusCode != http.StatusNotFound) {
		return requires, fmt.Errorf("read the rules of %s/%s's %s: %w", r.owner, r.repo, base, err)
	}
	if rules == nil {
		return requires, nil
	}
	for _, rule := range rules.RequiredStatusChecks {
		for _, check := range rule.Parameters.RequiredStatusChecks {
			var appID int64
			if check.IntegrationID != nil {
				appID = *check.IntegrationID
			}
			add(check.Context, appID)
		}
	}
	requires.linear = len(rules.RequiredLinearHistory) > 0
	for _, rule := range rules.PullRequest {
		for _, method := range rule.Parameters.AllowedMergeMethods {
			requires.methods = append(requires.methods, string(method))
		}
	}
	return requires, nil
}

// ready is the merge loop's verdict on head: every required check passed.
// A required check that failed ends the wait with its name.
func (r *requiredChecks) ready(ctx context.Context, head string) (bool, error) {
	if len(r.required) == 0 {
		r.say(fmt.Sprintf("#%d: the base branch requires no status check", r.number))
		return true, nil
	}
	combined, _, err := r.client.Repositories.GetCombinedStatus(ctx, r.owner, r.repo, head, &github.ListOptions{PerPage: 100})
	if err != nil {
		return false, fmt.Errorf("read the statuses of %s: %w", shortRevision(head), err)
	}
	var owed []string
	for _, check := range r.required {
		passed, failed, err := r.state(ctx, head, check, combined.Statuses)
		if err != nil {
			return false, err
		}
		if failed != "" {
			return false, fmt.Errorf("required check %s", failed)
		}
		if !passed {
			owed = append(owed, check.context)
		}
	}
	if len(owed) > 0 {
		r.waiting = "waiting on required check(s) " + strings.Join(owed, ", ")
		r.say(fmt.Sprintf("#%d: %s", r.number, r.waiting))
		return false, nil
	}
	r.waiting = ""
	r.say(fmt.Sprintf("#%d: every required check passed (%d)", r.number, len(r.required)))
	return true, nil
}

// state is one required check on head: passed, failed (with why), or still
// owed. A check run satisfies it when one by that name (from the app the
// requirement names, if it names one) has run; the newest such run is the
// verdict, as GitHub takes it. A commit status of that context does too.
func (r *requiredChecks) state(ctx context.Context, head string, check requiredCheck, statuses []*github.RepoStatus) (passed bool, failed string, err error) {
	options := &github.ListCheckRunsOptions{CheckName: github.Ptr(check.context), Filter: github.Ptr("latest"), ListOptions: github.ListOptions{PerPage: 100}}
	if check.appID > 0 {
		options.AppID = github.Ptr(check.appID)
	}
	runs, _, err := r.client.Checks.ListCheckRunsForRef(ctx, r.owner, r.repo, head, options)
	if err != nil {
		return false, "", fmt.Errorf("read check %s on %s: %w", check.context, shortRevision(head), err)
	}
	var newest *github.CheckRun
	if runs != nil {
		for _, run := range runs.CheckRuns {
			if newest == nil || run.GetID() > newest.GetID() {
				newest = run
			}
		}
	}
	if newest != nil {
		if newest.GetStatus() != "completed" {
			return false, "", nil
		}
		switch newest.GetConclusion() {
		case "success", "neutral", "skipped":
			return true, "", nil
		default:
			return false, fmt.Sprintf("%s concluded %s (%s)", check.context, newest.GetConclusion(), newest.GetHTMLURL()), nil
		}
	}
	for _, status := range statuses {
		if status.GetContext() != check.context {
			continue
		}
		switch status.GetState() {
		case "success":
			return true, "", nil
		case "failure", "error":
			return false, fmt.Sprintf("%s reported %s: %s (%s)", check.context, status.GetState(), status.GetDescription(), status.GetTargetURL()), nil
		}
		return false, "", nil
	}
	return false, "", nil
}

// mergeMethod picks how a promotion merges: a merge commit keeps the signed
// promotion commit on the base, so it is preferred unless the repository
// disallows it, the branch requires a linear history, or a ruleset allows
// other methods only.
func mergeMethod(ctx context.Context, client *github.Client, owner, repo string, requires baseRequirements) (string, error) {
	repository, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("read the merge methods %s/%s allows: %w", owner, repo, err)
	}
	allowed := func(method string) bool {
		return len(requires.methods) == 0 || slices.Contains(requires.methods, method)
	}
	switch {
	case repository.GetAllowMergeCommit() && !requires.linear && allowed("merge"):
		return "merge", nil
	case repository.GetAllowSquashMerge() && allowed("squash"):
		return "squash", nil
	case repository.GetAllowRebaseMerge() && allowed("rebase"):
		return "rebase", nil
	}
	return "", fmt.Errorf("%s/%s allows no merge method its rules admit for this promotion", owner, repo)
}
