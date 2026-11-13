package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v89/github"
)

// PullRequestMerge lands one pull request the way GitHub admits it: it waits
// until the pull request is mergeable and the caller's check policy says its
// head is ready, merges at that head, catches a branch up when a strict base
// moved past it, joins the merge queue when the base takes merges only
// through one, and asks GitHub whether a merge it could not confirm actually
// happened. It is the one merge loop the CLI has: a release lands its version
// bump through it, and a GitOps publication its promotion. What differs
// between them is which checks count (Ready) and how the merge is made.
type PullRequestMerge struct {
	Client      *github.Client
	Owner, Repo string
	Number      int
	// Noun names the pull request in errors and narration ("release pull
	// request", "promotion pull request").
	Noun string
	// Poll spaces out the reads while checks run. The overall budget is the
	// context's.
	Poll time.Duration
	// Ready reports whether the checks on sha admit the merge; an error is a
	// verdict that they never will (a check failed), and stops the merge.
	Ready func(ctx context.Context, sha string) (bool, error)
	// Method and CommitTitle are the merge's; empty uses GitHub's defaults.
	Method      string
	CommitTitle string
	// Head, when set, is the one head this merge may land: a pull request
	// whose head moved is refused rather than merged.
	Head string
	// MergeBlocked attempts the merge of a pull request GitHub reports
	// blocked once Ready admits its head, so the refusal names what blocks
	// it — a required review — instead of the wait running out on it.
	MergeBlocked bool
	// Say narrates a step; nil says nothing.
	Say func(string)
}

// StillOpenError is a merge whose budget ran out with the pull request open.
type StillOpenError struct {
	Number int
	State  string
	Err    error
}

func (e *StillOpenError) Error() string {
	return fmt.Sprintf("pull request #%d was still %q when the wait ran out: %v", e.Number, e.State, e.Err)
}

func (e *StillOpenError) Unwrap() error { return e.Err }

// Land merges the pull request and answers the commit its base carries for it.
func (m *PullRequestMerge) Land(ctx context.Context) (string, error) {
	say := m.Say
	if say == nil {
		say = func(string) {}
	}
	queued := false
	for {
		pr, _, err := m.Client.PullRequests.Get(ctx, m.Owner, m.Repo, m.Number)
		if err != nil {
			return "", fmt.Errorf("read %s #%d: %w", m.Noun, m.Number, err)
		}
		if pr.GetMerged() {
			return pr.GetMergeCommitSHA(), nil
		}
		if pr.GetState() == "closed" {
			return "", fmt.Errorf("%s #%d is closed and was not merged", m.Noun, m.Number)
		}
		head := pr.GetHead().GetSHA()
		if m.Head != "" && head != m.Head {
			return "", fmt.Errorf("%s #%d is at %s, not %s, the head this merge was asked to land", m.Noun, m.Number, short(head), short(m.Head))
		}
		state := pr.GetMergeableState()
		switch state {
		case "clean", "unstable", "has_hooks", "blocked":
			ready, readyErr := m.Ready(ctx, head)
			if readyErr != nil {
				return "", fmt.Errorf("%s #%d: %w", m.Noun, m.Number, readyErr)
			}
			if !ready || (state == "blocked" && !m.MergeBlocked && !queued) || queued {
				break
			}
			result, _, err := m.Client.PullRequests.Merge(ctx, m.Owner, m.Repo, m.Number, "", &github.PullRequestOptions{
				MergeMethod: m.Method, CommitTitle: m.CommitTitle, SHA: head,
			})
			if err != nil {
				// The merge can commit server-side and still fail the caller —
				// a dropped response or a 502 after the fact — so GitHub is
				// asked what happened instead of the transport.
				if commit, merged := m.confirmMerged(ctx); merged {
					return commit, nil
				}
				// A base owned by a merge queue refuses a direct merge; the
				// queue is then the only writer, so the pull request joins it
				// and this loop observes the merge the queue makes.
				if MergeQueueRequired(err) {
					if queueErr := Enqueue(ctx, m.Client, pr.GetNodeID()); queueErr != nil {
						return "", fmt.Errorf("add %s #%d to the merge queue: %w", m.Noun, m.Number, queueErr)
					}
					say(fmt.Sprintf("%s #%d entered the merge queue", m.Noun, m.Number))
					queued = true
					break
				}
				return "", fmt.Errorf("merge %s #%d: %w", m.Noun, m.Number, err)
			}
			if !result.GetMerged() {
				return "", fmt.Errorf("merge %s #%d: %s", m.Noun, m.Number, result.GetMessage())
			}
			return result.GetSHA(), nil
		case "dirty":
			return "", fmt.Errorf("%s #%d conflicts with %s", m.Noun, m.Number, pr.GetBase().GetRef())
		case "behind":
			// A strict base: a branch that fell behind never goes green
			// until it is caught up. A merge bound to one head cannot catch
			// up — GitHub's catch-up is a new head — so it says so instead.
			if m.Head != "" {
				return "", fmt.Errorf("%s #%d fell behind %s, which requires a branch to be up to date; catching it up would merge a head nobody pushed: publish again to rebuild it on %s", m.Noun, m.Number, pr.GetBase().GetRef(), pr.GetBase().GetRef())
			}
			var accepted *github.AcceptedError
			if _, _, err := m.Client.PullRequests.UpdateBranch(ctx, m.Owner, m.Repo, m.Number, nil); err != nil && !errors.As(err, &accepted) {
				return "", fmt.Errorf("update %s #%d onto %s: %w", m.Noun, m.Number, pr.GetBase().GetRef(), err)
			}
			say(fmt.Sprintf("%s #%d fell behind %s; caught it up", m.Noun, m.Number, pr.GetBase().GetRef()))
		}
		select {
		case <-ctx.Done():
			return "", &StillOpenError{Number: m.Number, State: state, Err: ctx.Err()}
		case <-time.After(m.Poll):
		}
	}
}

// confirmMerged asks whether the pull request is merged, on a context detached
// from the caller's budget so an expired deadline cannot turn a merge that
// succeeded into one reported as failed.
func (m *PullRequestMerge) confirmMerged(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	pr, _, err := m.Client.PullRequests.Get(ctx, m.Owner, m.Repo, m.Number)
	return pr.GetMergeCommitSHA(), err == nil && pr.GetMerged()
}

// MergeQueueRequired reports whether err is GitHub refusing a direct merge
// because a merge queue owns the base branch.
//
// The refusal is a 405 naming the queue. Matching the status alone would also
// swallow the other things GitHub answers 405 to, and matching the prose alone
// would depend on wording no API promises, so both are required.
func MergeQueueRequired(err error) bool {
	var response *github.ErrorResponse
	if !errors.As(err, &response) {
		return false
	}
	return response.Response != nil &&
		response.Response.StatusCode == http.StatusMethodNotAllowed &&
		strings.Contains(strings.ToLower(response.Message), "merge queue")
}

// Enqueue adds a pull request to its repository's merge queue.
//
// There is no REST route for this, so it is a GraphQL call through the same
// client — the same token and API host as every REST call.
// Enqueueing something already queued is not an error: the desired state is
// "in the queue", and it is.
func Enqueue(ctx context.Context, client *github.Client, nodeID string) error {
	if nodeID == "" {
		return errors.New("the pull request has no node id to enqueue")
	}
	body := map[string]any{
		"query":     "mutation($id:ID!){enqueuePullRequest(input:{pullRequestId:$id}){mergeQueueEntry{state}}}",
		"variables": map[string]any{"id": nodeID},
	}
	// api.github.com/graphql; <host>/api/graphql beside a GHES <host>/api/v3/.
	request, err := client.NewRequest(ctx, http.MethodPost, strings.TrimSuffix(client.BaseURL(), "v3/")+"graphql", body)
	if err != nil {
		return err
	}
	var answer struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := client.Do(request, &answer); err != nil {
		return err
	}
	if len(answer.Errors) == 0 {
		return nil
	}
	for _, reported := range answer.Errors {
		if strings.Contains(strings.ToLower(reported.Message), "already queued") {
			return nil
		}
	}
	return errors.New(answer.Errors[0].Message)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
