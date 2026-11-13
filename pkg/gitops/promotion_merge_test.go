package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const mergeHead = "1234567890123456789012345678901234567890"

// fakeDeliveryRepository is the slice of the GitHub API a promotion's merge
// reads: the pull request, the base branch's protection and rules, the head's
// check runs and statuses, the repository's merge methods, the merge itself,
// and the merge queue.
type fakeDeliveryRepository struct {
	mu sync.Mutex
	// protection is the base branch's required checks (branch protection).
	protection []string
	// rules are the base branch's rules, as the rules endpoint answers them.
	rules string
	// runs answers a check run by name, as (status, conclusion) per read: the
	// last entry repeats once the reads pass the list.
	runs map[string][][2]string
	// statuses are the head's commit statuses, context → state.
	statuses map[string]string
	// merged is set by the merge, with the method used.
	merged      bool
	method      string
	alreadyDone bool
	queued      int
	reads       map[string]int
	// mergeable is the pull request's mergeable state (default unstable);
	// queue makes a direct merge a 405 naming the merge queue; refusal is
	// any other 405 a merge answers; head is the pull request's head.
	mergeable string
	queue     bool
	refusal   string
	head      string
}

func (f *fakeDeliveryRepository) serve(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		switch {
		case r.Method == http.MethodGet && path == "/repos/owner/delivery/pulls/9":
			merged := f.merged || f.alreadyDone
			state, mergeable, head := "open", f.mergeable, f.head
			if merged {
				state = "closed"
			}
			if mergeable == "" {
				mergeable = "unstable"
			}
			if head == "" {
				head = mergeHead
			}
			fmt.Fprintf(w, `{"number":9,"node_id":"PR_9","state":%q,"merged":%t,"mergeable_state":%q,"merge_commit_sha":"feedfacefeedfacefeedfacefeedfacefeedface","head":{"sha":%q},"base":{"ref":"blue-delivery"}}`,
				state, merged, mergeable, head)
		case r.Method == http.MethodGet && path == "/repos/owner/delivery/branches/blue-delivery":
			checks, _ := json.Marshal(f.protection)
			fmt.Fprintf(w, `{"name":"blue-delivery","protection":{"enabled":true,"required_status_checks":{"contexts":%s}}}`, checks)
		case r.Method == http.MethodGet && path == "/repos/owner/delivery/rules/branches/blue-delivery":
			if f.rules == "" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprint(w, f.rules)
		case r.Method == http.MethodGet && path == "/repos/owner/delivery/commits/"+mergeHead+"/status":
			var statuses []string
			for context, state := range f.statuses {
				statuses = append(statuses, fmt.Sprintf(`{"context":%q,"state":%q}`, context, state))
			}
			fmt.Fprintf(w, `{"state":"pending","statuses":[%s]}`, strings.Join(statuses, ","))
		case r.Method == http.MethodGet && path == "/repos/owner/delivery/commits/"+mergeHead+"/check-runs":
			name := r.URL.Query().Get("check_name")
			states := f.runs[name]
			if len(states) == 0 {
				fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
				return
			}
			if f.reads == nil {
				f.reads = map[string]int{}
			}
			state := states[min(f.reads[name], len(states)-1)]
			f.reads[name]++
			fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":%q,"status":%q,"conclusion":%q,"html_url":"https://example.test/run"}]}`, name, state[0], state[1])
		case r.Method == http.MethodGet && path == "/repos/owner/delivery":
			fmt.Fprint(w, `{"allow_merge_commit":true,"allow_squash_merge":true,"allow_rebase_merge":false}`)
		case r.Method == http.MethodPut && path == "/repos/owner/delivery/pulls/9/merge":
			var body struct {
				SHA         string `json:"sha"`
				MergeMethod string `json:"merge_method"`
			}
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &body)
			if body.SHA != mergeHead {
				http.Error(w, `{"message":"Head branch was modified"}`, http.StatusConflict)
				return
			}
			if f.queue || f.refusal != "" {
				message := f.refusal
				if f.queue {
					message = "Changes must be made through the merge queue"
				}
				w.WriteHeader(http.StatusMethodNotAllowed)
				fmt.Fprintf(w, `{"message":%q}`, message)
				return
			}
			f.merged, f.method = true, body.MergeMethod
			fmt.Fprint(w, `{"merged":true,"sha":"cafecafecafecafecafecafecafecafecafecafe","message":"Pull Request successfully merged"}`)
		case r.Method == http.MethodPost && path == "/api/graphql":
			data, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(data), "enqueuePullRequest") || !strings.Contains(string(data), "PR_9") {
				http.Error(w, "unexpected query "+string(data), http.StatusInternalServerError)
				return
			}
			// The queue takes it, and merges it.
			f.queued++
			f.merged = true
			fmt.Fprint(w, `{"data":{"enqueuePullRequest":{"mergeQueueEntry":{"state":"QUEUED"}}}}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_API_URL", server.URL+"/api/v3/")
}

func mergeIt(t *testing.T) (MergeResult, []string, error) {
	t.Helper()
	var said []string
	result, err := MergePromotion(context.Background(), &PublishResult{
		Repository: "https://github.com/owner/delivery.git", BaseBranch: "blue-delivery",
		Commit: mergeHead, PullRequest: "https://github.com/owner/delivery/pull/9", PullRequestID: 9,
	}, MergeOptions{Interval: time.Millisecond, Timeout: 5 * time.Second, Progress: func(state string) { said = append(said, state) }})
	return result, said, err
}

// The 2026-11-13 Blue deploy: a check that is not required — a workflow about
// another environment — failed on every promotion, so the pull request read
// UNSTABLE and a poll for CLEAN timed out on promotions the repository would
// have merged. Only what the base branch requires is waited on.
func TestAPromotionMergesDespiteAFailingCheckItsBaseDoesNotRequire(t *testing.T) {
	repository := &fakeDeliveryRepository{runs: map[string][][2]string{"deploy-green": {{"completed", "failure"}}}}
	repository.serve(t)

	result, said, err := mergeIt(t)
	require.NoError(t, err)
	require.True(t, repository.merged)
	require.Equal(t, "merge", repository.method, "a merge commit keeps the signed promotion commit")
	require.Equal(t, "cafecafecafecafecafecafecafecafecafecafe", result.Commit)
	require.Contains(t, strings.Join(said, "\n"), "requires no status check")
}

// A required check is waited on until it passes, from branch protection and
// rulesets alike, and the wait is narrated once per change.
func TestAPromotionWaitsForTheChecksItsBaseRequires(t *testing.T) {
	repository := &fakeDeliveryRepository{
		protection: []string{"secret-scan"},
		rules:      `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"build"}]}}]`,
		runs: map[string][][2]string{
			"secret-scan":  {{"queued", ""}, {"in_progress", ""}, {"completed", "success"}},
			"deploy-green": {{"completed", "failure"}},
		},
		statuses: map[string]string{"build": "pending"},
	}
	repository.serve(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		repository.mu.Lock()
		repository.statuses["build"] = "success"
		repository.mu.Unlock()
	}()

	result, said, err := mergeIt(t)
	require.NoError(t, err)
	require.True(t, repository.merged)
	require.ElementsMatch(t, []string{"secret-scan", "build"}, result.Required)
	narrated := strings.Join(said, "\n")
	require.Contains(t, narrated, "waiting on required check(s) secret-scan, build")
	require.Contains(t, narrated, "every required check passed (2)")
	require.Less(t, len(said), 6, "the wait is narrated once per change, not once per read: %v", said)
}

// A required check that fails stops the merge, naming it.
func TestAFailingRequiredCheckStopsTheMerge(t *testing.T) {
	repository := &fakeDeliveryRepository{protection: []string{"secret-scan"}, runs: map[string][][2]string{"secret-scan": {{"completed", "failure"}}}}
	repository.serve(t)

	_, _, err := mergeIt(t)
	require.ErrorContains(t, err, "required check secret-scan concluded failure")
	require.False(t, repository.merged)
}

// A required check that never reports stops at the timeout, naming it.
func TestARequiredCheckThatNeverReportsTimesOut(t *testing.T) {
	repository := &fakeDeliveryRepository{protection: []string{"secret-scan"}}
	repository.serve(t)
	var said []string
	_, err := MergePromotion(context.Background(), &PublishResult{
		Repository: "https://github.com/owner/delivery.git", BaseBranch: "blue-delivery", Commit: mergeHead, PullRequestID: 9,
	}, MergeOptions{Interval: time.Millisecond, Timeout: 30 * time.Millisecond, Progress: func(state string) { said = append(said, state) }})
	require.ErrorContains(t, err, "waiting on required check(s) secret-scan")
	require.ErrorContains(t, err, "was not merged")
	require.False(t, repository.merged)
}

// A branch that requires a linear history takes the promotion squashed.
func TestALinearHistoryTakesTheSquash(t *testing.T) {
	repository := &fakeDeliveryRepository{rules: `[{"type":"required_linear_history"}]`}
	repository.serve(t)

	_, _, err := mergeIt(t)
	require.NoError(t, err)
	require.Equal(t, "squash", repository.method)
}

// A branch that merges through a queue refuses the direct merge once the
// required checks have passed on the head, and the promotion joins the queue;
// the merge is the queue's.
func TestAMergeQueueTakesThePromotion(t *testing.T) {
	repository := &fakeDeliveryRepository{queue: true, protection: []string{"secret-scan"},
		runs: map[string][][2]string{"secret-scan": {{"in_progress", ""}, {"completed", "success"}}}}
	repository.serve(t)

	result, said, err := mergeIt(t)
	require.NoError(t, err)
	require.Equal(t, 1, repository.queued)
	require.Equal(t, "feedfacefeedfacefeedfacefeedfacefeedface", result.Commit)
	narrated := strings.Join(said, "\n")
	require.Less(t, strings.Index(narrated, "every required check passed"), strings.Index(narrated, "entered the merge queue"),
		"the promotion joins the queue only once its head's required checks pass:\n%s", narrated)
}

// A merge GitHub refuses for a reason of its own — a review the base requires
// — stops with GitHub's reason, not with a wait that runs out.
func TestARequiredReviewStopsTheMergeWithItsReason(t *testing.T) {
	repository := &fakeDeliveryRepository{mergeable: "blocked", refusal: "At least 1 approving review is required by reviewers with write access."}
	repository.serve(t)

	_, _, err := mergeIt(t)
	require.ErrorContains(t, err, "approving review is required")
	require.False(t, repository.merged)
}

// A head that moved since the publish pushed it is never merged.
func TestAMovedHeadIsNotMerged(t *testing.T) {
	repository := &fakeDeliveryRepository{head: strings.Repeat("b", 40)}
	repository.serve(t)

	_, _, err := mergeIt(t)
	require.ErrorContains(t, err, "not 12345678, the head this merge was asked to land")
	require.False(t, repository.merged)
}

// A promotion already merged — a deploy run again after it stopped — answers
// its merge rather than failing.
func TestAMergedPromotionAnswersItsMerge(t *testing.T) {
	repository := &fakeDeliveryRepository{alreadyDone: true}
	repository.serve(t)

	result, _, err := mergeIt(t)
	require.NoError(t, err)
	require.Equal(t, "feedfacefeedfacefeedfacefeedfacefeedface", result.Commit)
}

// A local promotion is no pull request: there is nothing to merge.
func TestALocalPromotionIsNotMergedHere(t *testing.T) {
	_, err := MergePromotion(context.Background(), &PublishResult{Path: "environments/modules/payments", PullRequest: "/tmp/delivery#refs/codefly/reviews/x"}, MergeOptions{})
	require.ErrorContains(t, err, "not a GitHub pull request")
}

func TestDependencyOrderPutsEachModuleAfterWhatItDependsOn(t *testing.T) {
	dependsOn := map[string]map[string]bool{
		"wiki":      {"documents": true, "robin": true},
		"robin":     {"documents": true, "model": true},
		"documents": {"records": true},
		"a":         {"b": true},
		"b":         {"a": true},
	}
	require.Equal(t, []string{"model", "records", "documents", "robin", "wiki"},
		dependencyOrder([]string{"wiki", "robin", "model", "documents", "records"}, dependsOn))
	// A dependency outside the set orders nothing; a cycle waits for every free
	// module, then goes by name.
	require.Equal(t, []string{"wiki", "a", "b"}, dependencyOrder([]string{"wiki", "b", "a"}, dependsOn))
}

// The module hosting the delivery API is published last, after every module
// whose bindings it settles and withdraws; the rest go by their dependencies.
func TestPublishOrderPutsTheHostLast(t *testing.T) {
	ctx, workspace, env, _ := hostedPublishWorkspace(t)
	order, err := PublishOrder(ctx, workspace, env, []string{"payments", "zeta", "alpha"})
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "zeta", "payments"}, order)
}

// A promotion behind a strict base is not caught up on GitHub: the catch-up
// is a head nobody pushed, which the merge would then refuse. It says to
// publish again, and leaves the branch as it was.
func TestAPromotionBehindAStrictBaseIsNotRewritten(t *testing.T) {
	repository := &fakeDeliveryRepository{mergeable: "behind"}
	repository.serve(t)

	_, _, err := mergeIt(t)
	require.ErrorContains(t, err, "fell behind blue-delivery")
	require.ErrorContains(t, err, "publish again")
	require.False(t, repository.merged)
}
