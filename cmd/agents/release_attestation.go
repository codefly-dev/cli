package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/google/go-github/v89/github"
)

// Release evidence belongs to the CLI, alongside the unchanged Core CI report.
type releaseSourceReport struct {
	Path        string             `json:"path"`
	Reason      string             `json:"reason"`
	Attestation *sourceAttestation `json:"attestation,omitempty"`
}

type sourceAttestation struct {
	Workflow string `json:"workflow"`
	RunID    int64  `json:"run_id"`
	SHA      string `json:"sha"`
}

func (r *releaseSourceReport) summary() string {
	if a := r.Attestation; a != nil {
		return fmt.Sprintf("source tests skipped locally: attested by workflow %q, run %d, SHA %s", a.Workflow, a.RunID, a.SHA)
	}
	if r.Path == "local" {
		return "source tests run locally: " + r.Reason
	}
	return "source tests not run: " + r.Reason
}

func (t *releaseTarget) sourceAttestation(ctx context.Context, disabled bool) releaseSourceReport {
	local := func(reason string) releaseSourceReport { return releaseSourceReport{Path: "local", Reason: reason} }
	if disabled {
		return local("forced by --no-attestation")
	}
	// A dependency pin changes the worktree without changing HEAD. Such source
	// cannot inherit HEAD's evidence, even if the version bump has not happened.
	status, err := t.git(ctx, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return local(fmt.Sprintf("cannot verify clean checkout: %v", err))
	}
	if strings.TrimSpace(status) != "" {
		return local("checkout differs from HEAD")
	}
	head, err := t.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return local(fmt.Sprintf("cannot resolve HEAD: %v", err))
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return local("HEAD is empty")
	}
	client, err := gh.NewClient()
	if err != nil {
		return local(fmt.Sprintf("CI attestation unavailable: %v", err))
	}
	attestation, err := findSourceAttestation(ctx, client.Checks, client.Actions, t.owner, t.repo, head)
	if err != nil {
		return local(fmt.Sprintf("CI attestation unavailable: %v", err))
	}
	if attestation == nil {
		return local("no successful CI workflow for HEAD " + head)
	}
	return releaseSourceReport{Path: "attested", Reason: "successful CI workflow for exact HEAD", Attestation: attestation}
}

type checkRunsSource interface {
	ListCheckRunsForRef(context.Context, string, string, string, *github.ListCheckRunsOptions) (*github.ListCheckRunsResults, *github.Response, error)
}

type workflowRunsSource interface {
	ListRepositoryWorkflowRuns(context.Context, string, string, *github.ListWorkflowRunsOptions) (*github.WorkflowRuns, *github.Response, error)
}

// Check names are job names, not workflow names. Join their suite IDs to Actions
// runs so one passing job (or an unrelated workflow) cannot attest the suite.
func findSourceAttestation(ctx context.Context, checks checkRunsSource, actions workflowRunsSource, owner, repo, head string) (*sourceAttestation, error) {
	suites := make(map[int64]bool)
	checkOptions := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		result, response, err := checks.ListCheckRunsForRef(ctx, owner, repo, head, checkOptions)
		if err != nil {
			return nil, err
		}
		for _, check := range result.CheckRuns {
			if check.GetHeadSHA() == head && check.GetStatus() == "completed" && check.GetConclusion() == "success" && check.GetApp().GetSlug() == "github-actions" && check.GetCheckSuite().GetID() != 0 {
				suites[check.GetCheckSuite().GetID()] = true
			}
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		checkOptions.Page = response.NextPage
	}
	if len(suites) == 0 {
		return nil, nil
	}
	runOptions := &github.ListWorkflowRunsOptions{HeadSHA: head, ListOptions: github.ListOptions{PerPage: 100}}
	for {
		result, response, err := actions.ListRepositoryWorkflowRuns(ctx, owner, repo, runOptions)
		if err != nil {
			return nil, err
		}
		for _, run := range result.WorkflowRuns {
			if isCIWorkflow(run) && run.GetID() != 0 && run.GetName() != "" && run.GetHeadSHA() == head && run.GetStatus() == "completed" && run.GetConclusion() == "success" && suites[run.GetCheckSuiteID()] {
				return &sourceAttestation{Workflow: run.GetName(), RunID: run.GetID(), SHA: head}, nil
			}
		}
		if response == nil || response.NextPage == 0 {
			return nil, nil
		}
		runOptions.Page = response.NextPage
	}
}

func isCIWorkflow(run *github.WorkflowRun) bool {
	return strings.EqualFold(run.GetName(), "ci") || run.GetPath() == ".github/workflows/ci.yml" || run.GetPath() == ".github/workflows/ci.yaml"
}
