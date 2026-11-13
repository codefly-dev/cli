package gitops

import (
	"context"
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
// rulesets alike), passing on the promotion's head, and, where the branch
// takes merges through a queue, the queue. Nothing else is waited on.

// MergeOptions bounds and narrates a promotion's merge.
type MergeOptions struct {
	// Timeout bounds the wait for the required checks and the queue.
	Timeout time.Duration
	// Interval is how often the checks are read again (default 15s).
	Interval time.Duration
	// Progress is told each new state of what the merge waits on.
	Progress func(string)
}

// MergeResult is a merged promotion.
type MergeResult struct {
	Number int
	// Commit is the commit the base branch carries for the promotion.
	Commit string
	// Method is how it was merged: merge, squash, rebase, or queue.
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

// MergePromotion waits until every status check the promotion's base branch
// requires has passed on the promotion's head, then merges it — through the
// branch's merge queue when it has one — and answers the commit the base
// carries. A required check that fails, a merge the repository refuses (a
// review it requires, a base that moved under a strict policy) and a wait
// past the timeout are errors naming their reason; a promotion already merged
// answers its merge.
func MergePromotion(ctx context.Context, result *PublishResult, options MergeOptions) (MergeResult, error) {
	if result.PullRequestID == 0 {
		return MergeResult{}, fmt.Errorf("the promotion of %s is not a GitHub pull request (%s), so there is nothing to merge here", result.Path, result.PullRequest)
	}
	slug, err := validateRepositoryURL(result.Repository, false)
	if err != nil {
		return MergeResult{}, err
	}
	owner, repo, err := splitRepositorySlug(slug)
	if err != nil {
		return MergeResult{}, err
	}
	client, err := gh.NewClient()
	if err != nil {
		return MergeResult{}, err
	}
	merger := &promotionMerger{client: client, owner: owner, repo: repo, number: result.PullRequestID, options: options}
	if merger.options.Interval <= 0 {
		merger.options.Interval = 15 * time.Second
	}
	if merger.options.Progress == nil {
		merger.options.Progress = func(string) {}
	}
	if merger.options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, merger.options.Timeout)
		defer cancel()
	}
	return merger.merge(ctx, result.BaseBranch, result.Commit)
}

type promotionMerger struct {
	client  *github.Client
	owner   string
	repo    string
	number  int
	options MergeOptions
	last    string
}

func (m *promotionMerger) say(state string) {
	if state != m.last {
		m.last = state
		m.options.Progress(state)
	}
}

func (m *promotionMerger) merge(ctx context.Context, base, head string) (MergeResult, error) {
	merged := MergeResult{Number: m.number}
	pr, _, err := m.client.PullRequests.Get(ctx, m.owner, m.repo, m.number)
	if err != nil {
		return merged, fmt.Errorf("read promotion pull request #%d: %w", m.number, err)
	}
	if pr.GetMerged() {
		merged.Commit, merged.Method = pr.GetMergeCommitSHA(), "already merged"
		return merged, nil
	}
	if pr.GetState() != "open" {
		return merged, fmt.Errorf("promotion pull request #%d is %s, not open: publish again to open a new one", m.number, pr.GetState())
	}
	if pr.GetHead().GetSHA() != head {
		return merged, fmt.Errorf("promotion pull request #%d is at %s, not the commit this publish pushed (%s): publish again", m.number, shortRevision(pr.GetHead().GetSHA()), shortRevision(head))
	}
	required, queued, linear, err := m.requirements(ctx, base)
	if err != nil {
		return merged, err
	}
	for _, check := range required {
		merged.Required = append(merged.Required, check.context)
	}
	if queued {
		// A queue runs the required checks on the merge group itself, so the
		// head's are not waited on here: the queue is the gate.
		merged.Method = "queue"
		return merged, m.throughQueue(ctx, pr, &merged)
	}
	if err = m.awaitChecks(ctx, head, required); err != nil {
		return merged, err
	}
	if merged.Method, err = m.method(ctx, linear); err != nil {
		return merged, err
	}
	done, _, err := m.client.PullRequests.Merge(ctx, m.owner, m.repo, m.number, "", &github.PullRequestOptions{SHA: head, MergeMethod: merged.Method})
	if err != nil {
		return merged, fmt.Errorf("merge promotion pull request #%d: %w", m.number, err)
	}
	if !done.GetMerged() {
		return merged, fmt.Errorf("merge promotion pull request #%d: %s", m.number, done.GetMessage())
	}
	merged.Commit = done.GetSHA()
	return merged, nil
}

// requirements reads what the base branch requires of a merge: its required
// status checks (branch protection and rulesets), whether it merges through a
// queue, and whether it requires a linear history.
func (m *promotionMerger) requirements(ctx context.Context, base string) (required []requiredCheck, queued, linear bool, err error) {
	add := func(context string, appID int64) {
		if !slices.ContainsFunc(required, func(check requiredCheck) bool { return check.context == context }) {
			required = append(required, requiredCheck{context: context, appID: appID})
		}
	}
	branch, _, err := m.client.Repositories.GetBranch(ctx, m.owner, m.repo, base, 0)
	if err != nil {
		return nil, false, false, fmt.Errorf("read the protection of %s/%s's %s: %w", m.owner, m.repo, base, err)
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
	rules, _, err := m.client.Repositories.ListRulesForBranch(ctx, m.owner, m.repo, base, nil)
	if err != nil {
		return nil, false, false, fmt.Errorf("read the rules of %s/%s's %s: %w", m.owner, m.repo, base, err)
	}
	if rules != nil {
		for _, rule := range rules.RequiredStatusChecks {
			for _, check := range rule.Parameters.RequiredStatusChecks {
				var appID int64
				if check.IntegrationID != nil {
					appID = *check.IntegrationID
				}
				add(check.Context, appID)
			}
		}
		queued, linear = len(rules.MergeQueue) > 0, len(rules.RequiredLinearHistory) > 0
	}
	return required, queued, linear, nil
}

// checkState is one required check on the promotion's head: passed, failed
// (with why), or still owed.
func (m *promotionMerger) checkState(ctx context.Context, head string, check requiredCheck, statuses []*github.RepoStatus) (passed bool, failed string, err error) {
	options := &github.ListCheckRunsOptions{CheckName: github.Ptr(check.context), Filter: github.Ptr("latest")}
	if check.appID > 0 {
		options.AppID = github.Ptr(check.appID)
	}
	runs, _, err := m.client.Checks.ListCheckRunsForRef(ctx, m.owner, m.repo, head, options)
	if err != nil {
		return false, "", fmt.Errorf("read check %s on %s: %w", check.context, shortRevision(head), err)
	}
	if runs != nil && len(runs.CheckRuns) > 0 {
		run := runs.CheckRuns[0]
		if run.GetStatus() != "completed" {
			return false, "", nil
		}
		switch run.GetConclusion() {
		case "success", "neutral", "skipped":
			return true, "", nil
		default:
			return false, fmt.Sprintf("%s concluded %s (%s)", check.context, run.GetConclusion(), run.GetHTMLURL()), nil
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

// awaitChecks waits until every required check has passed on head, and
// refuses on the first that fails.
func (m *promotionMerger) awaitChecks(ctx context.Context, head string, required []requiredCheck) error {
	if len(required) == 0 {
		m.say(fmt.Sprintf("#%d: the base branch requires no status check", m.number))
		return nil
	}
	for {
		combined, _, err := m.client.Repositories.GetCombinedStatus(ctx, m.owner, m.repo, head, &github.ListOptions{PerPage: 100})
		if err != nil {
			return fmt.Errorf("read the statuses of %s: %w", shortRevision(head), err)
		}
		var owed []string
		for _, check := range required {
			passed, failed, checkErr := m.checkState(ctx, head, check, combined.Statuses)
			if checkErr != nil {
				return checkErr
			}
			if failed != "" {
				return fmt.Errorf("promotion pull request #%d: required check %s", m.number, failed)
			}
			if !passed {
				owed = append(owed, check.context)
			}
		}
		if len(owed) == 0 {
			m.say(fmt.Sprintf("#%d: every required check passed (%d)", m.number, len(required)))
			return nil
		}
		m.say(fmt.Sprintf("#%d: waiting on required check(s) %s", m.number, strings.Join(owed, ", ")))
		if err = sleep(ctx, m.options.Interval); err != nil {
			return fmt.Errorf("promotion pull request #%d: required check(s) %s did not pass in time: %w", m.number, strings.Join(owed, ", "), err)
		}
	}
}

// method picks how the repository lets a promotion merge: a merge commit
// keeps the signed promotion commit on the base, so it is preferred unless
// the repository disallows it or the branch requires a linear history.
func (m *promotionMerger) method(ctx context.Context, linear bool) (string, error) {
	repository, _, err := m.client.Repositories.Get(ctx, m.owner, m.repo)
	if err != nil {
		return "", fmt.Errorf("read the merge methods %s/%s allows: %w", m.owner, m.repo, err)
	}
	switch {
	case repository.GetAllowMergeCommit() && !linear:
		return "merge", nil
	case repository.GetAllowSquashMerge():
		return "squash", nil
	case repository.GetAllowRebaseMerge():
		return "rebase", nil
	}
	return "", fmt.Errorf("%s/%s allows no merge method this promotion can use", m.owner, m.repo)
}

// throughQueue adds the promotion to the base branch's merge queue and waits
// until the queue merges it, refusing when the queue drops it.
func (m *promotionMerger) throughQueue(ctx context.Context, pr *github.PullRequest, merged *MergeResult) error {
	var enqueued struct {
		Data struct {
			EnqueuePullRequest struct {
				MergeQueueEntry struct{ State string } `json:"mergeQueueEntry"`
			} `json:"enqueuePullRequest"`
		} `json:"data"`
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := m.graphQL(ctx, `mutation($id: ID!) { enqueuePullRequest(input: {pullRequestId: $id}) { mergeQueueEntry { state } } }`,
		map[string]any{"id": pr.GetNodeID()}, &enqueued); err != nil {
		return err
	}
	if len(enqueued.Errors) > 0 {
		return fmt.Errorf("add promotion pull request #%d to the merge queue: %s", m.number, enqueued.Errors[0].Message)
	}
	for {
		current, _, err := m.client.PullRequests.Get(ctx, m.owner, m.repo, m.number)
		if err != nil {
			return fmt.Errorf("read promotion pull request #%d: %w", m.number, err)
		}
		if current.GetMerged() {
			merged.Commit = current.GetMergeCommitSHA()
			return nil
		}
		var queued struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						MergeQueueEntry *struct{ State string } `json:"mergeQueueEntry"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err = m.graphQL(ctx, `query($owner: String!, $repo: String!, $number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $number) { mergeQueueEntry { state } } } }`,
			map[string]any{"owner": m.owner, "repo": m.repo, "number": m.number}, &queued); err != nil {
			return err
		}
		entry := queued.Data.Repository.PullRequest.MergeQueueEntry
		if entry == nil {
			return fmt.Errorf("promotion pull request #%d left the merge queue unmerged (a required check failed on its merge group, or it was removed)", m.number)
		}
		m.say(fmt.Sprintf("#%d: in the merge queue, %s", m.number, strings.ToLower(entry.State)))
		if err = sleep(ctx, m.options.Interval); err != nil {
			return fmt.Errorf("promotion pull request #%d was not merged by the queue in time: %w", m.number, err)
		}
	}
}

// graphQL runs one query against the API the client addresses.
func (m *promotionMerger) graphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	// api.github.com/graphql, or <host>/api/graphql beside <host>/api/v3/.
	endpoint := strings.TrimSuffix(m.client.BaseURL(), "v3/") + "graphql"
	request, err := m.client.NewRequest(ctx, http.MethodPost, endpoint, map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	if _, err = m.client.Do(request, out); err != nil {
		return fmt.Errorf("GitHub GraphQL: %w", err)
	}
	return nil
}

func sleep(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
