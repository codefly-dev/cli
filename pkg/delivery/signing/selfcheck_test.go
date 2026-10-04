package signing

import (
	"errors"
	"strings"
	"testing"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, present := values[name]
		return value, present
	}
}

// TestSelfCheckFromEnvironmentAdmitsExactlyThisWorkflow: the publisher's
// self-check is built from what GitHub Actions states of the job, and its
// policy admits that workflow at that ref and nothing else — the check a
// host holding the release workflow in its allowlist would make. The trusted
// root is fetched on first use, so building the check touches no network.
func TestSelfCheckFromEnvironmentAdmitsExactlyThisWorkflow(t *testing.T) {
	check, err := SelfCheckFromEnvironment(lookupFrom(map[string]string{
		envGitHubRepository:  "codefly-dev/cli",
		envGitHubWorkflowRef: "codefly-dev/cli/.github/workflows/release.yaml@refs/tags/v1.2.3",
	}))
	if err != nil || check == nil {
		t.Fatalf("SelfCheckFromEnvironment: check=%v err=%v", check != nil, err)
	}
	none, err := SelfCheckFromEnvironment(lookupFrom(map[string]string{}))
	if err != nil || none != nil {
		t.Fatalf("outside a workflow there is no self-check: check=%v err=%v", none != nil, err)
	}
	for name, values := range map[string]map[string]string{
		"no ref":             {envGitHubRepository: "codefly-dev/cli", envGitHubWorkflowRef: "codefly-dev/cli/.github/workflows/release.yaml"},
		"another repository": {envGitHubRepository: "codefly-dev/cli", envGitHubWorkflowRef: "codefly-dev/core/.github/workflows/release.yaml@refs/tags/v1"},
	} {
		if _, err := SelfCheckFromEnvironment(lookupFrom(values)); err == nil {
			t.Errorf("%s: a workflow ref the policy cannot be built from must be refused", name)
		}
	}
}

// TestSelfCheckPolicyIsTheWorkflowIdentity runs the policy the self-check
// builds against virtual releases: the job's own identity verifies, a
// sibling tag of the same workflow does not, and a bundle with no
// transparency evidence is refused by name — each as a host would decide.
func TestSelfCheckPolicyIsTheWorkflowIdentity(t *testing.T) {
	policy, err := GitHubActionsPolicy("codefly-dev/cli", ".github/workflows/release.yaml", "refs/tags/v1\\.2\\.3")
	if err != nil {
		t.Fatal(err)
	}
	release := signVirtually(t, releaseSubject, releaseIssuer, []byte("presence document"), false)
	if _, err := Verify(release.bundle, release.payload, release.trusted, policy); err != nil {
		t.Fatalf("the workflow's own release must verify: %v", err)
	}
	sibling := signVirtually(t, strings.Replace(releaseSubject, "v1.2.3", "v1.2.4", 1), releaseIssuer, []byte("presence document"), false)
	if _, err := Verify(sibling.bundle, sibling.payload, sibling.trusted, policy); !errors.Is(err, ErrSignature) {
		t.Fatalf("another ref of the same workflow must be refused: %v", err)
	}
	if _, err := Verify(withoutTransparencyLog(t, release.bundle), release.payload, release.trusted, policy); !errors.Is(err, ErrNoTransparency) {
		t.Fatalf("a bundle with no transparency evidence must be refused by name: %v", err)
	}
}

// TestSelfCheckUnderHoldsThisWorkflowToTheReviewedPolicy: the fresh-signature
// check is this workflow's exact identity, and it is built only when the
// reviewed policy admits that identity — another repository, another
// workflow, a branch or a tag outside the pattern never signs.
func TestSelfCheckUnderHoldsThisWorkflowToTheReviewedPolicy(t *testing.T) {
	policy := &ReleasePolicy{Repository: "codefly-dev/cli", Workflow: ".github/workflows/release.yaml", Refs: []string{`refs/tags/v[0-9]+\.[0-9]+\.[0-9]+`}}
	check, err := SelfCheckUnder(lookupFrom(map[string]string{
		envGitHubRepository: "codefly-dev/cli", envGitHubWorkflowRef: "codefly-dev/cli/.github/workflows/release.yaml@refs/tags/v1.2.3",
	}), policy)
	if err != nil || check == nil {
		t.Fatalf("an admitted identity: check=%v err=%v", check != nil, err)
	}
	for name, identity := range map[string][2]string{
		"another repository": {"codefly-dev/core", "codefly-dev/core/.github/workflows/release.yaml@refs/tags/v1.2.3"},
		"another workflow":   {"codefly-dev/cli", "codefly-dev/cli/.github/workflows/ci.yaml@refs/tags/v1.2.3"},
		"a branch":           {"codefly-dev/cli", "codefly-dev/cli/.github/workflows/release.yaml@refs/heads/main"},
		"a tag outside":      {"codefly-dev/cli", "codefly-dev/cli/.github/workflows/release.yaml@refs/tags/nightly"},
	} {
		if _, err := SelfCheckUnder(lookupFrom(map[string]string{envGitHubRepository: identity[0], envGitHubWorkflowRef: identity[1]}), policy); err == nil || !strings.Contains(err.Error(), "release policy") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	none, err := SelfCheckUnder(lookupFrom(map[string]string{}), policy)
	if err != nil || none != nil {
		t.Fatalf("no workflow metadata must build no check: check=%v err=%v", none != nil, err)
	}
	if _, err := ReleaseCheckUnder(nil); err == nil {
		t.Fatal("a reuse check needs the policy")
	}
}

// TestReleasePolicyAdmitsEveryReleaseRefAndNothingElse: a carrier delivered
// earlier is reused under the policy's refs — any release tag of the same
// workflow — and never under a branch of it.
func TestReleasePolicyAdmitsEveryReleaseRefAndNothingElse(t *testing.T) {
	pattern, err := (&ReleasePolicy{Repository: "codefly-dev/cli", Workflow: ".github/workflows/release.yaml", Refs: []string{`refs/tags/v[0-9]+\.[0-9]+\.[0-9]+`}}).refPattern()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := GitHubActionsPolicy("codefly-dev/cli", ".github/workflows/release.yaml", pattern)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v1.2.3", "v1.2.4"} {
		release := signVirtually(t, strings.Replace(releaseSubject, "v1.2.3", version, 1), releaseIssuer, []byte("presence document"), false)
		if _, err := Verify(release.bundle, release.payload, release.trusted, policy); err != nil {
			t.Fatalf("the release at %s must verify under the policy: %v", version, err)
		}
	}
	branch := signVirtually(t, strings.Replace(releaseSubject, "refs/tags/v1.2.3", "refs/heads/main", 1), releaseIssuer, []byte("presence document"), false)
	if _, err := Verify(branch.bundle, branch.payload, branch.trusted, policy); !errors.Is(err, ErrSignature) {
		t.Fatalf("a branch of the same workflow must be refused: %v", err)
	}
}
