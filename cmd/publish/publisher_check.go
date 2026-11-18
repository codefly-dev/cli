package publish

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/go-github/v89/github"
)

// publisherCheck resolves only this executing job, not every job in its
// workflow. A stranded release may share its SHA with this orchestration job;
// waiting for ourselves then cannot finish. Independent CI still qualifies it.
func (l *pullRequestLanding) publisherCheck(ctx context.Context, sha string) (int64, error) {
	if sha != os.Getenv("GITHUB_SHA") || os.Getenv("GITHUB_ACTIONS") != "true" || !strings.EqualFold(os.Getenv("GITHUB_REPOSITORY"), l.owner+"/"+l.repo) {
		return 0, nil
	}
	runID, err := strconv.ParseInt(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	if err != nil || runID <= 0 || os.Getenv("RUNNER_NAME") == "" {
		return 0, fmt.Errorf("cannot identify the publishing Actions job")
	}
	attempt, err := strconv.ParseInt(os.Getenv("GITHUB_RUN_ATTEMPT"), 10, 64)
	if err != nil || attempt <= 0 {
		return 0, fmt.Errorf("cannot identify the publishing Actions attempt")
	}
	options := &github.ListOptions{PerPage: 100}
	var checkID int64
	for {
		jobs, response, err := l.client.Actions.ListWorkflowJobsAttempt(ctx, l.owner, l.repo, runID, attempt, options)
		if err != nil {
			return 0, fmt.Errorf("identify publishing job: %w", err)
		}
		for _, job := range jobs.Jobs {
			if job.GetStatus() != "in_progress" || job.GetRunnerName() != os.Getenv("RUNNER_NAME") {
				continue
			}
			if job.GetHeadSHA() != sha {
				return 0, fmt.Errorf("publishing job does not match the checked commit")
			}
			if checkID != 0 {
				return 0, fmt.Errorf("multiple active Actions jobs match the publishing runner")
			}
			u, err := url.Parse(job.GetCheckRunURL())
			prefix := "/repos/" + l.owner + "/" + l.repo + "/check-runs/"
			if err != nil || !strings.HasPrefix(u.Path, prefix) {
				return 0, fmt.Errorf("publishing job has an invalid check identity")
			}
			checkID, err = strconv.ParseInt(strings.TrimPrefix(u.Path, prefix), 10, 64)
			if err != nil || checkID <= 0 {
				return 0, fmt.Errorf("publishing job has an invalid check ID")
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	if checkID == 0 {
		return 0, fmt.Errorf("no active Actions job matches the publishing runner")
	}
	return checkID, nil
}
