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
// ReleasePolicy is the identity a host accepts delivered carriers from, as
// the composition states it beside the host block and the host publishes it:
// one repository, one workflow path, and the refs that workflow may run at,
// each an anchored regular expression. It is reviewed with the composition,
// which is what makes it an allowlist rather than whatever identity the
// current process happens to run under.
type ReleasePolicy struct {
	Repository string
	Workflow   string
	Refs       []string
}

// refPattern is the policy's refs as one anchored alternation.
func (policy *ReleasePolicy) refPattern() (string, error) {
	if len(policy.Refs) == 0 {
		return "", fmt.Errorf("signing: the release policy names no ref")
	}
	for _, ref := range policy.Refs {
		if _, err := regexp.Compile("^(?:" + ref + ")$"); err != nil {
			return "", fmt.Errorf("signing: release policy ref %q is not a regular expression: %w", ref, err)
		}
	}
	return "(?:" + strings.Join(policy.Refs, "|") + ")", nil
}

// admits holds one workflow identity to the policy.
func (policy *ReleasePolicy) admits(repository, workflowPath, ref string) error {
	pattern, err := policy.refPattern()
	if err != nil {
		return err
	}
	switch {
	case repository != policy.Repository:
		return fmt.Errorf("signing: this workflow runs in repository %s and the environment's release policy admits %s only", repository, policy.Repository)
	case strings.TrimPrefix(workflowPath, "/") != strings.TrimPrefix(policy.Workflow, "/"):
		return fmt.Errorf("signing: this is workflow %s and the environment's release policy admits %s only", workflowPath, policy.Workflow)
	case !regexp.MustCompile("^" + pattern + "$").MatchString(ref):
		return fmt.Errorf("signing: this workflow runs at %s, which is outside the release policy's refs (%s); a release publish runs at a ref the host accepts", ref, strings.Join(policy.Refs, ", "))
	}
	return nil
}

// SelfCheckFromEnvironment is SelfCheckUnder with no policy: the current
// workflow identity, pinned exactly, held to nothing reviewed. A publish uses
// SelfCheckUnder; this is the shape a test seam needs.
func SelfCheckFromEnvironment(lookup func(string) (string, bool)) (SelfCheck, error) {
	return SelfCheckUnder(lookup, nil)
}

// SelfCheckUnder builds the check a release publish holds its FRESH carriers
// to: the workflow identity this process runs under (GitHub Actions states it
// to the job), pinned exactly — repository, workflow path and ref — and held
// to the reviewed policy first, so a workflow or a ref the host would refuse
// never signs. It returns no check (nil) where the workflow metadata is
// absent; what that means is the publish's to decide.
func SelfCheckUnder(lookup func(string) (string, bool), policy *ReleasePolicy) (SelfCheck, error) {
	repository, workflowPath, ref, present, err := workflowIdentity(lookup)
	if err != nil || !present {
		return nil, err
	}
	if policy != nil {
		if err := policy.admits(repository, workflowPath, ref); err != nil {
			return nil, err
		}
	}
	return workflowCheck(repository, workflowPath, regexp.QuoteMeta(ref), repository+"/"+workflowPath+"@"+ref)
}

// ReleaseCheckUnder builds the check a carrier delivered EARLIER is held to
// before it is reused: the policy's repository and workflow at any of its
// refs — the host's allowlist, not this process's identity.
func ReleaseCheckUnder(policy *ReleasePolicy) (SelfCheck, error) {
	if policy == nil {
		return nil, fmt.Errorf("signing: a delivered carrier is reused under the environment's release policy, and none was given")
	}
	pattern, err := policy.refPattern()
	if err != nil {
		return nil, err
	}
	return workflowCheck(policy.Repository, policy.Workflow, pattern, policy.Repository+"/"+policy.Workflow+"@"+strings.Join(policy.Refs, "|"))
}

// workflowIdentity reads the identity GitHub Actions states to the job.
func workflowIdentity(lookup func(string) (string, bool)) (repository, workflowPath, ref string, present bool, err error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	repository, _ = lookup(envGitHubRepository)
	workflowRef, _ := lookup(envGitHubWorkflowRef)
	if repository == "" || workflowRef == "" {
		return "", "", "", false, nil
	}
	path, ref, found := strings.Cut(workflowRef, "@")
	if !found || ref == "" {
		return "", "", "", false, fmt.Errorf("signing: %s=%q does not name a ref; expected <owner>/<repo>/<workflow path>@<ref>", envGitHubWorkflowRef, workflowRef)
	}
	workflowPath, found = strings.CutPrefix(path, repository+"/")
	if !found || workflowPath == "" {
		return "", "", "", false, fmt.Errorf("signing: %s=%q does not start with the repository %s", envGitHubWorkflowRef, workflowRef, repository)
	}
	return repository, workflowPath, ref, true, nil
}

// workflowCheck verifies a carrier offline, as a host will, under one GitHub
// Actions identity policy.
func workflowCheck(repository, workflowPath, refPattern, described string) (SelfCheck, error) {
	policy, err := GitHubActionsPolicy(repository, workflowPath, refPattern)
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
			return fmt.Errorf("the carrier does not verify as a host would verify it (identity %s against the public-good trusted root): %w", described, err)
		}
		return nil
	}, nil
}
