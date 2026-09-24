package publish

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/google/go-github/v89/github"
)

// defaultRemoteWorkflow is the caller workflow a repository adds to be
// released from GitHub (docs/commands.md, `codefly publish --remote`).
const defaultRemoteWorkflow = "publish.yml"

// remoteReleaseBudget bounds a wait on one dispatched release: the release
// pull request's checks, release-grade agent CI on the runner, and the
// repository's own release workflow — the same work a local publish does.
const remoteReleaseBudget = releaseWaitBudget + 45*time.Minute

// remoteRelease fires a repository's release workflow on GitHub instead of
// running the release here. The runner does the whole release — its Docker,
// its CI, the release pull request, the tag — so nothing about it depends on
// the machine that asked for it staying awake.
type remoteRelease struct {
	client   *github.Client
	owner    string
	repo     string
	workflow string
	poll     time.Duration
}

func newRemoteRelease(ctx context.Context, workDir, workflow string) (*remoteRelease, error) {
	if filepath.Base(workflow) != workflow || (filepath.Ext(workflow) != ".yml" && filepath.Ext(workflow) != ".yaml") {
		return nil, fmt.Errorf("--workflow must name a workflow file in .github/workflows, got %q", workflow)
	}
	owner, repo, err := originRepository(ctx, workDir)
	if err != nil {
		return nil, err
	}
	if gh.Token() == "" {
		return nil, fmt.Errorf("dispatching the release of %s/%s needs a GitHub token: set GITHUB_TOKEN or run `gh auth login`", owner, repo)
	}
	client, err := gh.NewClient()
	if err != nil {
		return nil, err
	}
	return &remoteRelease{client: client, owner: owner, repo: repo, workflow: workflow, poll: 20 * time.Second}, nil
}

func (r *remoteRelease) name() string { return r.owner + "/" + r.repo }

// check confirms the repository carries the release workflow on its default
// branch, so `publish all --remote` can refuse the whole run before it
// dispatches anything.
func (r *remoteRelease) check(ctx context.Context) error {
	_, resp, err := r.client.Actions.GetWorkflowByFileName(ctx, r.owner, r.repo, r.workflow)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%s has no .github/workflows/%s; add the caller workflow from docs/commands.md (`codefly publish --remote`) first", r.name(), r.workflow)
		}
		return fmt.Errorf("read %s workflow %s: %w", r.name(), r.workflow, err)
	}
	return nil
}

// dispatch starts the release workflow on main with the requested bump and
// returns the run it started.
func (r *remoteRelease) dispatch(ctx context.Context, bump string) (int64, string, error) {
	details, _, err := r.client.Actions.CreateWorkflowDispatchEventByFileName(ctx, r.owner, r.repo, r.workflow, github.CreateWorkflowDispatchEventRequest{
		Ref:              "main",
		Inputs:           map[string]any{"bump": bump},
		ReturnRunDetails: github.Ptr(true),
	})
	if err != nil {
		return 0, "", fmt.Errorf("dispatch %s on %s: %w", r.workflow, r.name(), err)
	}
	if details == nil || details.GetWorkflowRunID() == 0 {
		return 0, "", fmt.Errorf("dispatched %s on %s but GitHub returned no run to follow", r.workflow, r.name())
	}
	return details.GetWorkflowRunID(), details.GetHTMLURL(), nil
}

// wait follows a dispatched run to its conclusion. Only success is a release;
// every other conclusion names the run to read.
func (r *remoteRelease) wait(ctx context.Context, runID int64) error {
	for {
		run, _, err := r.client.Actions.GetWorkflowRunByID(ctx, r.owner, r.repo, runID)
		if err != nil {
			fmt.Printf("==> warning: cannot read %s run %d, still waiting: %v\n", r.name(), runID, err)
		} else if run.GetStatus() == runCompleted {
			if run.GetConclusion() != checkSuccess {
				return fmt.Errorf("release of %s concluded %s: %s", r.name(), run.GetConclusion(), run.GetHTMLURL())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("release of %s (run %d) had not finished when the wait ran out; it is still running on GitHub: %w", r.name(), runID, ctx.Err())
		case <-time.After(r.poll):
		}
	}
}

// validateRemoteBump admits the bumps the caller workflow's `bump` input offers.
// A beta is cut locally: its prerelease line is an operator decision, not a
// button.
func validateRemoteBump(bump string) error {
	switch bump {
	case bumpPatch, bumpMinor, bumpMajor:
		return nil
	default:
		return fmt.Errorf("--remote dispatches patch, minor or major, got %q", bump)
	}
}

// runRemote dispatches one repository's release and, when asked, waits for it.
func runRemote(workDir, bump, workflow string, wait bool) error {
	if err := validateRemoteBump(bump); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteReleaseBudget)
	defer cancel()
	remote, err := newRemoteRelease(ctx, workDir, workflow)
	if err != nil {
		return err
	}
	runID, url, err := remote.dispatch(ctx, bump)
	if err != nil {
		return err
	}
	fmt.Printf("==> %s release (%s) running on GitHub: %s\n", remote.name(), bump, url)
	if !wait {
		fmt.Println("==> nothing here needs to stay open; pass --wait to follow it to the end")
		return nil
	}
	if err := remote.wait(ctx, runID); err != nil {
		return err
	}
	fmt.Printf("==> %s released\n", remote.name())
	return nil
}
