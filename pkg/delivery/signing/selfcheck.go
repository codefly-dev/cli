package signing

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The workflow's identity, as GitHub Actions states it to the job: the
// repository and the workflow ref ("<owner>/<repo>/<path>@<ref>"), which is
// the subject Fulcio puts in the certificate the job signs with.
const (
	envGitHubRepository  = "GITHUB_REPOSITORY"
	envGitHubWorkflowRef = "GITHUB_WORKFLOW_REF"
)

// SelfCheck verifies, right after signing, that a bundle verifies the way a
// host will verify it: offline, against the trusted root, under the policy
// that admits this workflow's identity and no other. It is the publisher's
// own run of the verifier, so a bundle a host would refuse — a certificate
// whose identity is not the workflow's, a log entry the root cannot verify,
// a bundle carrying no transparency evidence — is refused at publish, in
// front of the person running it, rather than at the host.
type SelfCheck func(ctx context.Context, bundle, payload []byte) error

// SelfCheckFromEnvironment builds the publisher's self-check from the GitHub
// Actions job environment, or returns nil when the process is not such a job
// (lookup nil = os.LookupEnv). The policy admits exactly this job's workflow
// identity — repository, workflow path and ref, each quoted — so the check
// proves what a host holding the release workflow's identity in its allowlist
// would prove. The trusted root is the public-good root fetched through TUF on
// first use, which is the root a host mirrors.
func SelfCheckFromEnvironment(lookup func(string) (string, bool)) (SelfCheck, error) {
	return workflowCheck(lookup, regexp.QuoteMeta)
}

// ReleaseCheckFromEnvironment is the check a carrier signed by an EARLIER
// release passes: the same repository and workflow path as this one, under
// any release tag. A carrier delivered by release tag A is carried forward by
// release tag B unchanged, as a host whose allowlist names the workflow and
// the tag pattern accepts it; a carrier from a branch run, or from another
// workflow, does not pass and is re-signed by this release.
func ReleaseCheckFromEnvironment(lookup func(string) (string, bool)) (SelfCheck, error) {
	return workflowCheck(lookup, func(string) string { return releaseRefPattern })
}

// releaseRefPattern admits every release tag of the repository.
const releaseRefPattern = `refs/tags/[^/]+`

func workflowCheck(lookup func(string) (string, bool), refPattern func(ref string) string) (SelfCheck, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	repository, _ := lookup(envGitHubRepository)
	workflowRef, _ := lookup(envGitHubWorkflowRef)
	if repository == "" || workflowRef == "" {
		return nil, nil
	}
	// <owner>/<repo>/<workflow path>@<ref>
	path, ref, found := strings.Cut(workflowRef, "@")
	if !found || ref == "" {
		return nil, fmt.Errorf("signing: %s=%q does not name a ref; expected <owner>/<repo>/<workflow path>@<ref>", envGitHubWorkflowRef, workflowRef)
	}
	workflowPath, found := strings.CutPrefix(path, repository+"/")
	if !found || workflowPath == "" {
		return nil, fmt.Errorf("signing: %s=%q does not start with the repository %s", envGitHubWorkflowRef, workflowRef, repository)
	}
	policy, err := GitHubActionsPolicy(repository, workflowPath, refPattern(ref))
	if err != nil {
		return nil, err
	}
	var once sync.Once
	var trusted *root.TrustedRoot
	var fetchErr error
	return func(ctx context.Context, bundle, payload []byte) error {
		once.Do(func() { trusted, fetchErr = PublicGoodTrustedRoot(ctx) })
		if fetchErr != nil {
			return fetchErr
		}
		if _, err := Verify(bundle, payload, trusted, policy); err != nil {
			return fmt.Errorf("the carrier signed by this workflow does not verify as a host would verify it (identity %s/%s@%s against the public-good trusted root): %w", repository, workflowPath, ref, err)
		}
		return nil
	}, nil
}
