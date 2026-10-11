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

// TestALocalPromotionSettlesARebuildAtTheNextGeneration is the local
// promotion rule: on a k3d environment whose host lists developers, the
// developer's signed publish advances the local base branch itself, so the
// next publish settles against it — a rebuilt module lands at generation 2
// with its new digest, never at a second generation 1 the host would refuse
// as a rewrite — and both commits fast-forward the base, the second
// descending from the first.
func TestALocalPromotionSettlesARebuildAtTheNextGeneration(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{nonce: true}})
	request := PublishRequest{Module: "payments", Environment: "production", Local: true, PromotionBranch: "codefly/promote-payments-production"}
	publish := func() (PublishPlan, PublishResult) {
		t.Helper()
		plan, err := PlanPublish(ctx, workspace, &request)
		if err != nil {
			t.Fatal(err)
		}
		result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
		if err != nil {
			t.Fatal(err)
		}
		return plan, result
	}
	first, firstResult := publish()
	if !first.SelfPromotes || !firstResult.Promoted {
		t.Fatalf("a developer-signed local publish did not promote itself: plan %v result %v", first.SelfPromotes, firstResult.Promoted)
	}
	if base := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main"); base != firstResult.Commit {
		t.Fatalf("the local base branch is %s, want the promoted commit %s", base, firstResult.Commit)
	}

	env := selectedEnvironment(t, workspace, "production")
	renderHostedPayments(t, workspace, env, strings.ReplaceAll(pinnedDeployment, "aaaa", "bbbb"), hostedPaymentsInstances())
	second, secondResult := publish()
	if g1, g2 := first.Delivery.Documents[0].Generation, second.Delivery.Documents[0].Generation; g1 != 1 || g2 != 2 {
		t.Fatalf("a rebuilt module settled at generations %d then %d, want 1 then 2", g1, g2)
	}
	if first.Delivery.Documents[0].Digest == second.Delivery.Documents[0].Digest {
		t.Fatal("the rebuilt module's document did not change, so this test proves nothing")
	}
	if base := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main"); base != secondResult.Commit {
		t.Fatalf("the second promotion left the base at %s, want %s", base, secondResult.Commit)
	}
	gitRun(t, "", "--git-dir", remote, "merge-base", "--is-ancestor", firstResult.Commit, secondResult.Commit)

	// What a cluster run reads back: the latest promotion is the second
	// commit, and the tree it promoted renders the presence Job that POSTs
	// the generation-2 carrier.
	latest, err := LatestLocalPromotion(ctx, workspace, "production")
	if err != nil || latest != secondResult.Commit {
		t.Fatalf("latest local promotion %s (%v), want %s", latest, err, secondResult.Commit)
	}
	tree := filepath.Join(t.TempDir(), "tree")
	if err = ExportPromotion(ctx, workspace, "production", latest, tree); err != nil {
		t.Fatal(err)
	}
	jobs, err := DeliveryJobsOf(tree, secondResult.Path, "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Kind != deliveryPresence || jobs[0].Namespace != "payments" || !strings.HasPrefix(jobs[0].Name, "deliver-presence-payments-production-") {
		t.Fatalf("the promoted tree's delivery Jobs are %+v", jobs)
	}

	// Running it again with nothing rebuilt re-delivers what is promoted: the
	// same generation, the same commit, no refusal for having no changes.
	again, againResult := publish()
	if again.Delivery.Documents[0].Generation != 2 || againResult.Commit != secondResult.Commit {
		t.Fatalf("an unchanged re-run settled at generation %d on %s, want 2 on %s", again.Delivery.Documents[0].Generation, againResult.Commit, secondResult.Commit)
	}

	// Another promotion branch (another module's, in a composition) moves the
	// base past this one: this branch is then contained in the base, starts
	// again from it, and its promotion fast-forwards — never forced, never a
	// divergence.
	request.PromotionBranch = "codefly/promote-other-production"
	renderHostedPayments(t, workspace, env, strings.ReplaceAll(pinnedDeployment, "aaaa", "dddd"), hostedPaymentsInstances())
	third, thirdResult := publish()
	if third.Delivery.Documents[0].Generation != 3 {
		t.Fatalf("a promotion on another branch settled at generation %d, want 3", third.Delivery.Documents[0].Generation)
	}
	request.PromotionBranch = "codefly/promote-payments-production"
	fourth, fourthResult := publish()
	if fourth.Delivery.Documents[0].Generation != 3 || fourthResult.Commit != thirdResult.Commit {
		t.Fatalf("a branch the base moved past settled at generation %d on %s, want 3 on %s", fourth.Delivery.Documents[0].Generation, fourthResult.Commit, thirdResult.Commit)
	}
	if branch := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/codefly/promote-payments-production"); branch != thirdResult.Commit {
		t.Fatalf("the promotion branch the base moved past was left at %s, want %s", branch, thirdResult.Commit)
	}
}

// TestAFailedVerificationPromotesNothing: the promotion is the last step of a
// publish whose every carrier passed the developer check. A carrier the check
// refuses at publish — the plan's, held again — stops the publish before any
// commit lands, and the base branch stays where it was.
func TestAFailedVerificationPromotesNothing(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	fake := &developerSigning{signer: &fakeSigner{nonce: true}}
	installDeveloperSigning(t, fake)
	before := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main")
	request := PublishRequest{Module: "payments", Environment: "production", Local: true, PromotionBranch: "codefly/promote-payments-production"}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	fake.refuse = errors.New("the certificate names someone@example.test")
	if _, err = Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit); err == nil {
		t.Fatal("a publish whose carrier fails the developer check succeeded")
	}
	if after := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main"); after != before {
		t.Fatalf("a failed verification moved the local base branch from %s to %s", before, after)
	}
}

// TestOnlyADeveloperSignedLocalPublishSelfPromotes: a local publish to a host
// listing no developer keeps the review — its base branch moves only by a
// merge — and so does every publish that is not --local, whatever the
// environment lists.
func TestOnlyADeveloperSignedLocalPublishSelfPromotes(t *testing.T) {
	ctx, workspace, _, remote := hostedPublishWorkspace(t)
	before := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main")
	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{},
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SelfPromotes || result.Promoted {
		t.Fatal("a local publish to a host listing no developer promoted itself")
	}
	if after := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main"); after != before {
		t.Fatalf("the base branch moved from %s to %s without a merge", before, after)
	}

	_, developerWorkspace, _ := developerPublishWorkspace(t)
	env := selectedEnvironment(t, developerWorkspace, "production")
	if (&deliveryPublication{local: false}).selfPromotes(env) {
		t.Fatal("a publish that is not --local would promote itself")
	}
	if !(&deliveryPublication{local: true}).selfPromotes(env) {
		t.Fatal("a developer-signed local publish would not promote itself")
	}
}

// TestDeliveryReceiptsAreTheJobsAnswers: a run reports what the host answered
// for each document — delivered with its receipt, refused with its verdict,
// given up on — and leaves the retry chatter out.
func TestDeliveryReceiptsAreTheJobsAnswers(t *testing.T) {
	log := strings.Join([]string{
		"delivery API answered 503 for a.json in round 1; it is tried again next round",
		`delivered a.json: {"status":"accepted","generation":2}`,
		"refused b.json with 409: rewritten generation",
		"giving up on c.json after 9 rounds within 1500 seconds",
		"delivered 1 document(s) to http://host:8080/platform/_delivery/presence in 9 round(s), 2 refused",
	}, "\n")
	got := DeliveryReceipts(log)
	if len(got) != 4 || !strings.HasPrefix(got[0], "delivered a.json") || !strings.HasPrefix(got[1], "refused b.json") || !strings.HasPrefix(got[2], "giving up on c.json") || !strings.HasPrefix(got[3], "delivered 1 document(s)") {
		t.Fatalf("receipts %q", got)
	}
}
