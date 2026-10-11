package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

const localDeveloper = "      developers:\n        - issuer: https://github.com/login/oauth\n          subject: dev@example.test\n"

// developerPublishWorkspace is hostedPublishWorkspace with the k3d
// environment's host listing one developer: the identity the LOCAL host's
// allowlist names, under which a local run's documents are signed.
func developerPublishWorkspace(t *testing.T) (context.Context, *resources.Workspace, string) {
	t.Helper()
	ctx, workspace, _, remote := hostedPublishWorkspace(t)
	path := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	anchor := "          - refs/tags/v[0-9]+[.][0-9]+[.][0-9]+\n"
	withDeveloper := strings.Replace(string(config), anchor, anchor+localDeveloper, 1)
	if withDeveloper == string(config) {
		t.Fatal("the fixture's host block was not found")
	}
	if err = os.WriteFile(path, []byte(withDeveloper), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err = resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return ctx, workspace, remote
}

// developerSigning stands in for the developer signer and its check: the
// real ones reach Fulcio, Rekor and the public-good trusted root. It records
// the policy and token source the publish built them from, and every check.
type developerSigning struct {
	signer   signing.Signer
	policies []*signing.DeveloperPolicy
	sources  []signing.IDTokenSource
	checked  int
	refuse   error
}

func installDeveloperSigning(t *testing.T, fake *developerSigning) {
	t.Helper()
	signer, check := developerSigner, developerCheck
	t.Cleanup(func() { developerSigner, developerCheck = signer, check })
	developerSigner = func(policy *signing.DeveloperPolicy, source signing.IDTokenSource, _ signing.KeylessOptions) (signing.Signer, error) {
		fake.policies = append(fake.policies, policy)
		fake.sources = append(fake.sources, source)
		return fake.signer, nil
	}
	developerCheck = func(policy *signing.DeveloperPolicy) (signing.SelfCheck, error) {
		return func(context.Context, []byte, []byte) error {
			fake.checked++
			return fake.refuse
		}, nil
	}
}

// TestLocalPublishSignsAsTheDeveloperAndDeliversThroughTheJobs is cli#918's
// path: a local qualification publish to an environment whose host lists a
// developer signs every document under the developer's identity — the
// developer signer, built from host.developers and the request's token
// source — checks each fresh carrier as the local host will, and writes the
// SAME carriers and Jobs a cell receives. The publish executing the plan
// reuses the plan's carriers (held to the same check), so one login signs
// one promotion.
func TestLocalPublishSignsAsTheDeveloperAndDeliversThroughTheJobs(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	fake := &developerSigning{signer: &fakeSigner{nonce: true}}
	installDeveloperSigning(t, fake)
	source := signing.IDTokenSource(func(context.Context) (string, error) { return "a.b.c", nil })
	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, DeveloperIDToken: source,
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Delivery == nil || !plan.Delivery.Signed {
		t.Fatalf("a local publish to a host listing a developer did not sign: %+v", plan.Delivery)
	}
	if len(fake.policies) == 0 {
		t.Fatal("the developer signer was never built")
	}
	policy := fake.policies[0]
	if len(policy.Developers) != 1 || policy.Developers[0] != (signing.DeveloperIdentity{Issuer: "https://github.com/login/oauth", Subject: "dev@example.test"}) {
		t.Fatalf("the developer signer was built from %+v, not from host.developers", policy)
	}
	if fake.sources[0] == nil {
		t.Fatal("the request's token source did not reach the developer signer")
	}
	signer, ok := fake.signer.(*fakeSigner)
	if !ok || signer.signed != 1 || fake.checked != 1 {
		t.Fatalf("the plan signed %d documents and checked %d carriers, want the one presence document each", signer.signed, fake.checked)
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if signer.signed != 1 {
		t.Fatalf("the publish signed again (%d signatures): the plan's carriers were not reused", signer.signed)
	}
	if fake.checked < 2 {
		t.Fatal("the publish reused the plan's carrier without holding it to the developer check")
	}
	overlay := result.Path + "/" + solutionHostBindingDir + "/overlays/production/"
	job := gitOutput(t, "", "--git-dir", remote, "show", result.Commit+":"+overlay+"deliver-presence.yaml")
	for _, want := range []string{"serviceAccountName: delivery", "audience: accounts", "http://host.payments.svc.cluster.local:8080", "/platform/_delivery/presence"} {
		if !strings.Contains(job, want) {
			t.Fatalf("the local publish's Job lacks %q:\n%s", want, job)
		}
	}
	inventory := gitOutput(t, "", "--git-dir", remote, "show", result.Commit+":"+result.Path+"/"+InventoryFilename)
	if !strings.Contains(inventory, `"signed": true`) {
		t.Fatalf("the published inventory does not record a signed delivery:\n%s", inventory)
	}
}

// TestLocalPublishAsADeveloperNeverFallsBackToUnsigned: once the host lists
// developers, an unsigned local delivery is a refusal, not a quieter
// default — a sign-in that failed, or a carrier the local host would refuse,
// stops the publish with the reason, and nothing is written for the cluster.
func TestLocalPublishAsADeveloperNeverFallsBackToUnsigned(t *testing.T) {
	ctx, workspace, _ := developerPublishWorkspace(t)
	request := PublishRequest{Module: "payments", Environment: "production", Local: true, PromotionBranch: "codefly/promote-payments-production"}

	installDeveloperSigning(t, &developerSigning{signer: &signing.Unavailable{Reason: "the browser login was abandoned"}})
	if _, err := PlanPublish(ctx, workspace, &request); !errors.Is(err, signing.ErrNoIdentity) || !strings.Contains(err.Error(), "browser login was abandoned") {
		t.Fatalf("a developer publish with no token was not refused with the reason: %v", err)
	}

	refused := errors.New("the certificate names someone@example.test")
	installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{}, refuse: refused})
	if _, err := PlanPublish(ctx, workspace, &request); !errors.Is(err, refused) {
		t.Fatalf("a carrier the developer check refuses was delivered: %v", err)
	}
}

// TestADeveloperIdentityNeverSignsARelease: host.developers shapes a --local
// publish only. A release publish to the same environment is held to its
// release workflow as before — with no workflow identity it refuses, and the
// developer signer is never built.
func TestADeveloperIdentityNeverSignsARelease(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	fake := &developerSigning{signer: &fakeSigner{}}
	installDeveloperSigning(t, fake)
	// A release publish is refused a file repository, so the fixture's bare
	// repository answers for a GitHub one, as in the release tests.
	repository := "https://github.com/codefly-test/manifests.git"
	gitopsDefaults, err := environments.WorkspaceGitops(workspace)
	if err != nil {
		t.Fatal(err)
	}
	gitopsDefaults.RepoURL = repository
	setWorkspaceGitops(t, workspace, gitopsDefaults)
	configured, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	if err != nil {
		configured = 0
	}
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(configured+1))
	t.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", configured), "url.file://"+remote+".insteadOf")
	t.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", configured), repository)
	for _, name := range []string{"GITHUB_REPOSITORY", "GITHUB_WORKFLOW_REF"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	request := PublishRequest{Module: "payments", Environment: "production", PromotionBranch: "codefly/promote-payments-production"}
	if _, err = PlanPublish(ctx, workspace, &request); err == nil || !strings.Contains(err.Error(), "run it from the release workflow") {
		t.Fatalf("a release publish was not held to the release workflow: %v", err)
	}
	if len(fake.policies) != 0 {
		t.Fatal("a release publish built the developer signer")
	}
}
