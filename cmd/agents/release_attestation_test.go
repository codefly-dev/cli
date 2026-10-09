package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	civ0 "github.com/codefly-dev/core/generated/go/codefly/ci/v0"
	"github.com/google/go-github/v89/github"
)

// HTTP is the check-runs source seam, exercising the shared authenticated client
// and the actual SHA/repository query as well as the source-stage decision.
func TestReleaseSourceAttestation(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	for _, test := range []struct {
		name                                       string
		change                                     func(*github.CheckRun, *github.WorkflowRun)
		disabled, dirty, apiError, empty, wantSkip bool
	}{
		{name: "attested", wantSkip: true},
		{name: "no checks", empty: true},
		{name: "different check SHA", change: func(c *github.CheckRun, _ *github.WorkflowRun) { c.HeadSHA = github.Ptr("other") }},
		{name: "different workflow SHA", change: func(_ *github.CheckRun, r *github.WorkflowRun) { r.HeadSHA = github.Ptr("other") }},
		{name: "failed workflow", change: func(_ *github.CheckRun, r *github.WorkflowRun) { r.Conclusion = github.Ptr("failure") }},
		{name: "pending workflow", change: func(_ *github.CheckRun, r *github.WorkflowRun) { r.Status = github.Ptr("in_progress") }},
		{name: "skipped check", change: func(c *github.CheckRun, _ *github.WorkflowRun) { c.Conclusion = github.Ptr("skipped") }},
		{name: "unrelated workflow", change: func(_ *github.CheckRun, r *github.WorkflowRun) {
			r.Name = github.Ptr("Release")
			r.Path = github.Ptr(".github/workflows/release.yml")
		}},
		{name: "different suite", change: func(_ *github.CheckRun, r *github.WorkflowRun) { r.CheckSuiteID = github.Ptr(int64(99)) }},
		{name: "other app", change: func(c *github.CheckRun, _ *github.WorkflowRun) { c.App.Slug = github.Ptr("other") }},
		{name: "renamed CI", wantSkip: true, change: func(_ *github.CheckRun, r *github.WorkflowRun) { r.Name = github.Ptr("Agent checks") }},
		{name: "CI named workflow", wantSkip: true, change: func(_ *github.CheckRun, r *github.WorkflowRun) { r.Path = github.Ptr(".github/workflows/go.yml") }},
		{name: "forced local", disabled: true},
		{name: "pin changed checkout", dirty: true},
		{name: "API unavailable", apiError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := &github.CheckRun{HeadSHA: github.Ptr(head), Status: github.Ptr("completed"), Conclusion: github.Ptr("success"), App: &github.App{Slug: github.Ptr("github-actions")}, CheckSuite: &github.CheckSuite{ID: github.Ptr(int64(11))}}
			run := &github.WorkflowRun{ID: github.Ptr(int64(42)), Name: github.Ptr("ci"), Path: github.Ptr(".github/workflows/ci.yml"), HeadSHA: github.Ptr(head), Status: github.Ptr("completed"), Conclusion: github.Ptr("success"), CheckSuiteID: github.Ptr(int64(11))}
			if test.change != nil {
				test.change(check, run)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if test.apiError {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				switch r.URL.Path {
				case "/api/v3/repos/example/agent/commits/" + head + "/check-runs":
					checks := []*github.CheckRun{check}
					if test.empty {
						checks = nil
					}
					_ = json.NewEncoder(w).Encode(github.ListCheckRunsResults{CheckRuns: checks})
				case "/api/v3/repos/example/agent/actions/runs":
					if r.URL.Query().Get("head_sha") != head {
						t.Errorf("SHA query = %s", r.URL.RawQuery)
					}
					_ = json.NewEncoder(w).Encode(github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{run}})
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			useFakeGitHub(t, server)
			oldGit := gitRun
			gitRun = func(_ context.Context, dir string, args ...string) (string, error) {
				if dir != "/agent" {
					t.Fatalf("wrong checkout %s", dir)
				}
				switch args[0] {
				case "status":
					if test.dirty {
						return " M go.mod", nil
					}
					return "", nil
				case "rev-parse":
					return head, nil
				default:
					return "", fmt.Errorf("unexpected git command %v", args)
				}
			}
			t.Cleanup(func() { gitRun = oldGit })
			target := &releaseTarget{workDir: "/agent", owner: "example", repo: "agent"}
			state := &agentCIState{started: time.Now(), report: &civ0.AgentCIReport{Summary: &civ0.AgentCISummary{}, Stages: []*civ0.AgentCIStage{{Name: stageSource, Status: statusPending}}}}
			prepared, validated := 0, 0
			var evidence releaseSourceReport
			err := state.runSourceStage(context.Background(), func(ctx context.Context) bool {
				evidence = target.sourceAttestation(ctx, test.disabled)
				return evidence.Attestation != nil
			}, func() error { prepared++; return nil }, func() error { validated++; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if prepared != 1 {
				t.Fatalf("prepare = %d", prepared)
			}
			if test.wantSkip {
				if validated != 0 || state.stage(stageSource).Status != statusSkipped {
					t.Fatalf("attested source ran: validations=%d, stage=%v", validated, state.stage(stageSource))
				}
				if evidence.Path != "attested" || evidence.Attestation.Workflow != run.GetName() || evidence.Attestation.RunID != 42 || evidence.Attestation.SHA != head {
					t.Fatalf("evidence = %+v", evidence)
				}
				payload, err := json.Marshal(evidence)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{`"run_id":42`, `"sha":"` + head + `"`, `"workflow":"` + run.GetName() + `"`} {
					if !strings.Contains(string(payload), want) {
						t.Fatalf("report missing %s: %s", want, payload)
					}
				}
				body := releasePRBody(releaseOptions{}, "0.1.0", "v0.1.1", &evidence)
				if !strings.Contains(body, head) || !strings.Contains(body, "run 42") {
					t.Fatalf("PR omitted evidence: %s", body)
				}
			} else {
				if validated != 1 || state.stage(stageSource).Status != statusPassed || evidence.Path != "local" || evidence.Reason == "" || evidence.Attestation != nil {
					t.Fatalf("local run: validations=%d, stage=%v, report=%+v", validated, state.stage(stageSource), evidence)
				}
			}
			if (test.disabled || test.dirty) && calls != 0 {
				t.Fatalf("unneeded API calls: %d", calls)
			}
		})
	}
}

func TestSourceStagePreservesFailuresAndStandaloneValidation(t *testing.T) {
	for _, test := range []struct {
		name                                  string
		attested, prepareError, validateError bool
	}{
		{name: "standalone"},
		{name: "local failure", validateError: true},
		{name: "attested preparation failure", attested: true, prepareError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &agentCIState{report: &civ0.AgentCIReport{Stages: []*civ0.AgentCIStage{{Name: stageSource, Status: statusPending}}}}
			var lookup func(context.Context) bool
			if test.attested {
				lookup = func(context.Context) bool { return true }
			}
			calls := 0
			err := state.runSourceStage(context.Background(), lookup, func() error {
				if test.prepareError {
					return fmt.Errorf("prepare failed")
				}
				return nil
			}, func() error {
				calls++
				if test.validateError {
					return fmt.Errorf("tests failed")
				}
				return nil
			})
			wantError := test.prepareError || test.validateError
			if (err != nil) != wantError {
				t.Fatalf("error = %v", err)
			}
			if wantError && state.stage(stageSource).Status != statusFailed {
				t.Fatal("failure was hidden")
			}
			if !test.attested && calls != 1 {
				t.Fatal("standalone validation skipped")
			}
		})
	}
}

type checkRunsFunc func(*github.ListCheckRunsOptions) (*github.ListCheckRunsResults, *github.Response, error)

func (f checkRunsFunc) ListCheckRunsForRef(_ context.Context, _, _, _ string, options *github.ListCheckRunsOptions) (*github.ListCheckRunsResults, *github.Response, error) {
	return f(options)
}

type workflowRunsFunc func(*github.ListWorkflowRunsOptions) (*github.WorkflowRuns, *github.Response, error)

func (f workflowRunsFunc) ListRepositoryWorkflowRuns(_ context.Context, _, _ string, options *github.ListWorkflowRunsOptions) (*github.WorkflowRuns, *github.Response, error) {
	return f(options)
}

func TestSourceAttestationPaginatesChecksAndWorkflows(t *testing.T) {
	checkPages, runPages := 0, 0
	checks := checkRunsFunc(func(options *github.ListCheckRunsOptions) (*github.ListCheckRunsResults, *github.Response, error) {
		checkPages++
		if checkPages == 1 {
			return &github.ListCheckRunsResults{}, &github.Response{NextPage: 2}, nil
		}
		if checkPages != 2 || options.Page != 2 {
			t.Fatalf("unexpected check page %d", options.Page)
		}
		return &github.ListCheckRunsResults{CheckRuns: []*github.CheckRun{{
			HeadSHA: github.Ptr("head"), Status: github.Ptr("completed"), Conclusion: github.Ptr("success"),
			App: &github.App{Slug: github.Ptr("github-actions")}, CheckSuite: &github.CheckSuite{ID: github.Ptr(int64(11))},
		}}}, &github.Response{}, nil
	})
	workflows := workflowRunsFunc(func(options *github.ListWorkflowRunsOptions) (*github.WorkflowRuns, *github.Response, error) {
		runPages++
		if options.HeadSHA != "head" {
			t.Fatalf("head SHA = %s", options.HeadSHA)
		}
		if runPages == 1 {
			return &github.WorkflowRuns{}, &github.Response{NextPage: 2}, nil
		}
		if runPages != 2 || options.Page != 2 {
			t.Fatalf("unexpected workflow page %d", options.Page)
		}
		return &github.WorkflowRuns{WorkflowRuns: []*github.WorkflowRun{{
			ID: github.Ptr(int64(42)), Name: github.Ptr("CI"), HeadSHA: github.Ptr("head"),
			Status: github.Ptr("completed"), Conclusion: github.Ptr("success"), CheckSuiteID: github.Ptr(int64(11)),
		}}}, &github.Response{}, nil
	})
	attestation, err := findSourceAttestation(context.Background(), checks, workflows, "owner", "repo", "head")
	if err != nil || attestation == nil || attestation.RunID != 42 || checkPages != 2 || runPages != 2 {
		t.Fatalf("attestation=%+v, error=%v, pages=%d/%d", attestation, err, checkPages, runPages)
	}
}
