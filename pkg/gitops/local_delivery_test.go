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
	"github.com/codefly-dev/core/solutionhost"
)

const localDeveloper = "      developers:\n        - key_id: dev-0123456789abcdef\n"

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
	anchor := "        key_id: release-v1\n"
	withDeveloper := strings.Replace(string(config), anchor, anchor+localDeveloper, 1)
	if withDeveloper == string(config) {
		t.Fatal("the fixture's host block was not found")
	}
	// A developer signs only for a DECLARED local cluster: kind and context.
	cluster := "  - name: production\n    namespace: payments\n    cluster:\n      kind: k3d\n"
	declared := strings.Replace(withDeveloper, cluster, cluster+"      context: k3d-dev\n", 1)
	if declared == withDeveloper {
		t.Fatal("the fixture's cluster block was not found")
	}
	withDeveloper = declared
	if err = os.WriteFile(path, []byte(withDeveloper), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err = resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return ctx, workspace, remote
}

// developerSigning supplies custody without touching the machine keychain.
type developerSigning struct {
	signer signing.Signer
	loaded int
	refuse error
}

func installDeveloperSigning(t *testing.T, fake *developerSigning) {
	t.Helper()
	before := developerSigner
	t.Cleanup(func() { developerSigner = before })
	developerSigner = func() (solutionhost.Signer, error) { fake.loaded++; return fake.signer, fake.refuse }
}

// TestLocalPublishSignsAsTheDeveloperAndDeliversThroughTheJobs is cli#918's
// path: a local qualification publish to an environment whose host lists a
// developer signs every document under the developer's identity — the
// developer key, admitted by host.developers — checks each fresh carrier as the local host will, and writes the
// SAME carriers and Jobs a cell receives. The publish executing the plan
// reuses the plan's carriers (held to the same check), so one key signs
// one promotion.
func TestLocalPublishSignsAsTheDeveloperAndDeliversThroughTheJobs(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	fake := &developerSigning{signer: &fakeSigner{nonce: true}}
	installDeveloperSigning(t, fake)
	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true,
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Delivery == nil || !plan.Delivery.Signed {
		t.Fatalf("a local publish to a host listing a developer did not sign: %+v", plan.Delivery)
	}
	if fake.loaded == 0 {
		t.Fatal("developer custody was never loaded")
	}
	signer := fake.signer.(*fakeSigner)
	if signer.signed != 1 {
		t.Fatalf("the plan signed %d documents, want the one module manifest", signer.signed)
	}
	for _, raw := range plan.Carriers {
		carrier, err := solutionhost.ParseSigned(raw)
		if err != nil {
			t.Fatal(err)
		}
		check, err := signing.KeyCheck(signer, solutionhost.KeyKindPerson, "example", signer.KeyID())
		if err != nil {
			t.Fatal(err)
		}
		if err = check(ctx, carrier.Envelope, carrier.Document); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if signer.signed != 1 {
		t.Fatalf("the publish signed again (%d signatures): the plan's carriers were not reused", signer.signed)
	}
	overlay := result.Path + "/" + moduleManifestDir + "/overlays/production/"
	job := gitOutput(t, "", "--git-dir", remote, "show", result.Commit+":"+overlay+"deliver-manifest.yaml")
	for _, want := range []string{"serviceAccountName: delivery", "namespace: payments", "audience: accounts", "http://host.payments.svc.cluster.local:8080", manifestDeliveryPath} {
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

	installDeveloperSigning(t, &developerSigning{refuse: fmt.Errorf("custody unavailable: %w", signing.ErrNoIdentity)})
	if _, err := PlanPublish(ctx, workspace, &request); !errors.Is(err, signing.ErrNoIdentity) || !strings.Contains(err.Error(), "custody unavailable") {
		t.Fatalf("a developer publish with no token was not refused with the reason: %v", err)
	}

	refused := errors.New("custody could not load the key")
	installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{}, refuse: refused})
	if _, err := PlanPublish(ctx, workspace, &request); !errors.Is(err, refused) {
		t.Fatalf("a carrier the developer check refuses was delivered: %v", err)
	}
}

// TestADeveloperIdentityNeverSignsARelease: host.developers shapes a --local
// publish only. A release publish to the same environment is held to its
// release key — with no release custody it refuses, and the
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
	for _, name := range []string{"VAULT_ADDR", "VAULT_TOKEN", "CODEFLY_RELEASE_TRANSIT_KEY", "CODEFLY_RELEASE_KMS_KEY"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	request := PublishRequest{Module: "payments", Environment: "production", PromotionBranch: "codefly/promote-payments-production"}
	if _, err = PlanPublish(ctx, workspace, &request); err == nil || !strings.Contains(err.Error(), "VAULT_ADDR") {
		t.Fatalf("a release publish was not held to release custody: %v", err)
	}
	if fake.loaded != 0 {
		t.Fatal("a release publish built the developer signer")
	}
}

// TestAPublishDeliversUnderTheGitopsPathExactlyAsRendered: a workspace
// delivering into its own repository declares `path: deployments`, the
// directory its render already writes `modules/<module>` under. The publish
// delivers to exactly that tree — never `deployments/deployments/modules`,
// the doubled path obin-ai/platform-obin#128 committed, which the cell's
// ApplicationSet never globs — and the next publish reads the prior
// generation back from the same tree, so a changed manifest settles at
// generation 2 against generation 1 rather than against nothing. (A rebuild
// alone is no change: a manifest names no build since core#812, so the change
// here is the container that authenticates.)
func TestAPublishDeliversUnderTheGitopsPathExactlyAsRendered(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	configPath := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	declared := strings.Replace(string(config), "  path: environments\n", "  path: deployments\n", 1)
	if declared == string(config) {
		t.Fatal("the fixture's gitops path was not found")
	}
	if err = os.WriteFile(configPath, []byte(declared), 0o644); err != nil {
		t.Fatal(err)
	}
	if workspace, err = resources.LoadWorkspaceFromDir(ctx, workspace.Dir()); err != nil {
		t.Fatal(err)
	}
	env := selectedEnvironment(t, workspace, "production")
	renderHostedPayments(t, workspace, env, pinnedDeployment, hostedPaymentsInstances())
	inventory, err := LoadInventory(filepath.Join(workspace.Dir(), "deployments", "modules", "payments"))
	if err != nil {
		t.Fatal(err)
	}
	if inventory.OwnedPath != "deployments/modules/payments" {
		t.Fatalf("the render owns %q, want the workspace's own deployments/modules/payments", inventory.OwnedPath)
	}
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
	if first.Path != "deployments/modules/payments" || firstResult.Path != first.Path {
		t.Fatalf("the publish delivered to %q (result %q), want deployments/modules/payments", first.Path, firstResult.Path)
	}
	delivered := gitOutput(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", "refs/heads/main")
	if !strings.Contains(delivered, "deployments/modules/payments/"+InventoryFilename+"\n") {
		t.Fatalf("the promoted tree holds no inventory at deployments/modules/payments:\n%s", delivered)
	}
	if strings.Contains(delivered, "deployments/deployments/") {
		t.Fatalf("the promoted tree prefixes the gitops path twice:\n%s", delivered)
	}

	renderHostedPayments(t, workspace, env, strings.Replace(pinnedDeployment, "- name: api\n", "- name: api-v2\n", 1), hostedPaymentsInstances())
	second, _ := publish()
	if g1, g2 := first.Delivery.Documents[0].Generation, second.Delivery.Documents[0].Generation; g1 != 1 || g2 != 2 {
		t.Fatalf("a changed module settled at generations %d then %d, want 1 then 2", g1, g2)
	}
}

// TestALocalPromotionSettlesAChangedManifestAtTheNextGeneration is the local
// promotion rule: on a k3d environment whose host lists developers, the
// developer's signed publish advances the local base branch itself, so the
// next publish settles against it — a module whose manifest changed (here the
// container that authenticates) lands at generation 2, never at a second
// generation 1 the host would refuse as a rewrite — and both commits
// fast-forward the base, the second descending from the first.
func TestALocalPromotionSettlesAChangedManifestAtTheNextGeneration(t *testing.T) {
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
	renderHostedPayments(t, workspace, env, strings.Replace(pinnedDeployment, "- name: api\n", "- name: api-v2\n", 1), hostedPaymentsInstances())
	second, secondResult := publish()
	if g1, g2 := first.Delivery.Documents[0].Generation, second.Delivery.Documents[0].Generation; g1 != 1 || g2 != 2 {
		t.Fatalf("a changed module settled at generations %d then %d, want 1 then 2", g1, g2)
	}
	if first.Delivery.Documents[0].Digest == second.Delivery.Documents[0].Digest {
		t.Fatal("the module's manifest did not change, so this test proves nothing")
	}
	if base := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main"); base != secondResult.Commit {
		t.Fatalf("the second promotion left the base at %s, want %s", base, secondResult.Commit)
	}
	gitRun(t, "", "--git-dir", remote, "merge-base", "--is-ancestor", firstResult.Commit, secondResult.Commit)

	// What a cluster run reads back: the latest promotion is the second
	// commit, and the tree it promoted renders the one Job that POSTs the
	// generation-2 manifest carrier.
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
	if len(jobs) != 1 || jobs[0].Kind != deliveryManifest || jobs[0].Namespace != "payments" || !strings.HasPrefix(jobs[0].Name, "deliver-manifest-payments-production-") {
		t.Fatalf("the promoted tree's delivery Jobs are %+v", jobs)
	}

	// Running it again with nothing changed re-delivers what is promoted: the
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
	renderHostedPayments(t, workspace, env, strings.Replace(pinnedDeployment, "- name: api\n", "- name: api-v3\n", 1), hostedPaymentsInstances())
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
	fake.refuse = errors.New("custody could not load the key")
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
	// The legacy reading (an environment NAMED local, no cluster kind) is
	// never a local cluster here: not for promotion, not for signing.
	developers := []environments.HostDeveloper{{KeyID: "dev-0123456789abcdef"}}
	for name, cluster := range map[string]*environments.EnvironmentCluster{
		"no cluster":            nil,
		"a non-k3d context":     {Context: "gke-production"},
		"a kind and no context": {Kind: environments.ClusterKindK3d},
	} {
		legacy := &environments.Environment{Name: "local", Cluster: cluster, Host: &environments.EnvironmentHost{Developers: developers}}
		publication := &deliveryPublication{local: true}
		if publication.selfPromotes(legacy) {
			t.Errorf("%s: an undeclared cluster would self-promote", name)
		}
		if err := publication.signAsDeveloper(legacy); err == nil || !strings.Contains(err.Error(), "declares no local k3d cluster") {
			t.Errorf("%s: an undeclared cluster would sign as a developer: %v", name, err)
		}
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

// TestALocalPromotionAdvancesTheDeclaredRepositoryWhateverGitIsConfiguredToDo
// is the attack: ambient git configuration rewrites the declared file:// URL
// (insteadOf, pushInsteadOf, injected through GIT_CONFIG_* the way any global
// configuration would) to ANOTHER repository. The promotion acts on the
// declared repository directly, through no configuration but its own: the
// declared base advances to the signed commit, the rewritten destination's
// base never moves.
func TestALocalPromotionAdvancesTheDeclaredRepositoryWhateverGitIsConfiguredToDo(t *testing.T) {
	for _, rewrite := range []string{"insteadOf", "pushInsteadOf"} {
		t.Run(rewrite, func(t *testing.T) {
			ctx, workspace, declared := developerPublishWorkspace(t)
			installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{nonce: true}})
			redirected := filepath.Join(t.TempDir(), "other.git")
			gitRun(t, "", "clone", "--quiet", "--bare", declared, redirected)
			before := gitOutput(t, "", "--git-dir", declared, "rev-parse", "main")
			count, _ := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
			t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(count+1))
			t.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", count), "url.file://"+redirected+"."+rewrite)
			// The rewrite names the repository as the publish reaches it
			// (resolved, since the clone reads the bound repository by its
			// resolved path), so every git read and write is rewritten.
			resolved, err := filepath.EvalSymlinks(declared)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", count), "file://"+resolved)

			request := PublishRequest{Module: "payments", Environment: "production", Local: true}
			plan, err := PlanPublish(ctx, workspace, &request)
			if err != nil {
				t.Fatal(err)
			}
			result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
			// Whatever the publish concludes, the rewritten destination's base
			// never moves, and a promotion it reports is the declared base.
			if got := gitOutput(t, "", "--git-dir", redirected, "rev-parse", "main"); got != before {
				t.Fatalf("the rewritten destination's base moved from %s to %s", before, got)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := gitOutput(t, "", "--git-dir", declared, "rev-parse", "main"); !result.Promoted || got != result.Commit {
				t.Fatalf("declared base = %s, want %s (promoted=%v)", got, result.Commit, result.Promoted)
			}
		})
	}
}

// TestALocalPromotionIsACompareAndSwapOnTheSettledBase: the base ref moves
// only from the base the plan settled its generations against. A base that
// moved in between — another promotion — refuses this one, and the other's
// commit stays.
func TestALocalPromotionIsACompareAndSwapOnTheSettledBase(t *testing.T) {
	ctx, workspace, declared := developerPublishWorkspace(t)
	installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{nonce: true}})
	request := PublishRequest{Module: "payments", Environment: "production", Local: true, PromotionBranch: "codefly/promote-payments-production"}
	prepared, err := preparePublish(ctx, workspace, &request, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.cleanup()
	gitRun(t, prepared.dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "--quiet", "--allow-empty", "-m", "promotion")
	commit := gitOutput(t, prepared.dir, "rev-parse", "HEAD")
	// Another promotion lands first.
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "--quiet", declared, other)
	gitRun(t, other, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "--quiet", "--allow-empty", "-m", "the other promotion")
	gitRun(t, other, "push", "--quiet", "origin", "HEAD:main")
	landed := gitOutput(t, "", "--git-dir", declared, "rev-parse", "main")
	if err = promoteLocally(ctx, prepared, commit); err == nil || !strings.Contains(err.Error(), "base moved") {
		t.Fatalf("a promotion over a base that moved was not refused: %v", err)
	}
	if got := gitOutput(t, "", "--git-dir", declared, "rev-parse", "main"); got != landed {
		t.Fatalf("the other promotion %s was overwritten by %s", landed, got)
	}
}

// TestADeliveryJobNamesTheApplicationTheCLIRendered: the Application a local
// run may ask for a delivery is read from the ApplicationSet the CLI itself
// rendered — the name its template gives the in-cluster tenant's component,
// the repository and revision it syncs, its destination server and namespace
// — so the run can hold the cluster's Application to all of it.
func TestADeliveryJobNamesTheApplicationTheCLIRendered(t *testing.T) {
	tree := t.TempDir()
	module := "env/modules/payments"
	overlay := module + "/" + moduleManifestDir + "/overlays/local"
	if err := os.MkdirAll(filepath.Join(tree, filepath.FromSlash(overlay)), 0o755); err != nil {
		t.Fatal(err)
	}
	job := "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: deliver-manifest-payments-local-abc\n  namespace: payments\n  labels:\n    codefly.dev/delivery: carrier\n    codefly.dev/delivered-document: manifest\n"
	if err := os.WriteFile(filepath.Join(tree, filepath.FromSlash(overlay), "deliver-"+deliveryManifest+".yaml"), []byte(job), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file of a retired delivery kind is no Job the run awaits.
	if err := os.WriteFile(filepath.Join(tree, filepath.FromSlash(overlay), "deliver-presence.yaml"), []byte(job), 0o644); err != nil {
		t.Fatal(err)
	}
	component := argoBoundedName(componentNameBudget, "payments", moduleManifestDir)
	if err := os.MkdirAll(filepath.Join(tree, filepath.FromSlash(module), "bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeArgoApplicationSet(filepath.Join(tree, filepath.FromSlash(module), "bootstrap", "applicationset.yaml"),
		"payments-local", "payments", "https://fetch.example.test/repo.git", "0123456789abcdef0123456789abcdef01234567",
		"tenants/local", "payments", []argoBootstrapComponent{{Component: component, Overlay: overlay, Wave: "1"}}); err != nil {
		t.Fatal(err)
	}
	jobs, err := DeliveryJobsOf(tree, module, "local")
	if err != nil {
		t.Fatal(err)
	}
	want := DeliveryApplication{
		Name: "in-cluster-" + component, RepoURL: "https://fetch.example.test/repo.git",
		TargetRevision: "0123456789abcdef0123456789abcdef01234567", Path: overlay,
		Server: "https://kubernetes.default.svc", Namespace: "payments",
		Owner: ApplicationSetIdentity{APIVersion: argoAPIVersion, Kind: kindApplicationSet, Name: "payments-local", Namespace: "argocd"},
	}
	if len(jobs) != 1 || jobs[0].Kind != deliveryManifest || jobs[0].Application != want {
		t.Fatalf("jobs %+v, want the manifest Job naming the Application %+v", jobs, want)
	}
}

// TestALocalPromotionFollowsTheRepositoryPathAsTheFilesystemDoes is the
// attack: the declared repository is reached through a directory link and a
// "..", file://<base>/alias/../payments.git. The filesystem — and git, for
// every other step of the publish — follows alias to its target and takes ".."
// from there; a lexical clean names <base>/payments.git, another repository
// beside the link. The promotion advances the repository the filesystem
// reaches, and the lexical sibling never moves.
func TestALocalPromotionFollowsTheRepositoryPathAsTheFilesystemDoes(t *testing.T) {
	ctx, workspace, declared := developerPublishWorkspace(t)
	installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{nonce: true}})
	if err := os.MkdirAll(filepath.Join(filepath.Dir(declared), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	if err := os.Symlink(filepath.Join(filepath.Dir(declared), "sub"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(base, filepath.Base(declared))
	gitRun(t, "", "clone", "--quiet", "--bare", declared, decoy)
	before := gitOutput(t, "", "--git-dir", decoy, "rev-parse", "main")
	gitopsDefaults, err := environments.WorkspaceGitops(workspace)
	if err != nil {
		t.Fatal(err)
	}
	gitopsDefaults.RepoURL = "file://" + filepath.Join(base, "alias") + "/../" + filepath.Base(declared)
	setWorkspaceGitops(t, workspace, gitopsDefaults)

	request := PublishRequest{Module: "payments", Environment: "production", Local: true}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, "", "--git-dir", declared, "rev-parse", "main"); !result.Promoted || got != result.Commit {
		t.Fatalf("the repository the path reaches is at %s, want the promoted %s", got, result.Commit)
	}
	if got := gitOutput(t, "", "--git-dir", decoy, "rev-parse", "main"); got != before {
		t.Fatalf("the lexical sibling moved from %s to %s", before, got)
	}
}

// TestReviewECFRepositoryReplacementAfterPlan inverts the
// review's ECF probe: after the plan, the declared repository is replaced —
// its link retargeted at another repository, or another directory renamed
// into its place — with the same history, so ancestry and the compare-and-
// swap would pass. The plan is bound to the repository on disk (resolved
// path, device and inode), so the publish refuses the plan as stale, nothing
// moves anywhere; and a replacement between a publish's own binding and its
// promotion is refused at the promotion.
func TestReviewECFRepositoryReplacementAfterPlan(t *testing.T) {
	for _, replacement := range []string{"retarget-symlink", "replace-directory"} {
		t.Run(replacement, func(t *testing.T) {
			ctx, workspace, declared := developerPublishWorkspace(t)
			installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{nonce: true}})
			decoy := filepath.Join(t.TempDir(), "decoy.git")
			gitRun(t, "", "clone", "--quiet", "--bare", declared, decoy)
			before := gitOutput(t, "", "--git-dir", declared, "rev-parse", "main")
			alias := filepath.Join(t.TempDir(), "declared.git")
			if err := os.Symlink(declared, alias); err != nil {
				t.Fatal(err)
			}
			config, err := environments.WorkspaceGitops(workspace)
			if err != nil {
				t.Fatal(err)
			}
			config.RepoURL = "file://" + alias
			setWorkspaceGitops(t, workspace, config)
			request := PublishRequest{Module: "payments", Environment: "production", Local: true}
			plan, err := PlanPublish(ctx, workspace, &request)
			if err != nil {
				t.Fatal(err)
			}
			original, replaced := declared, decoy
			replace := func() {
				t.Helper()
				if replacement == "retarget-symlink" {
					if err := os.Remove(alias); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(decoy, alias); err != nil {
						t.Fatal(err)
					}
					return
				}
				original = declared + ".original"
				if err := os.Rename(declared, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(decoy, declared); err != nil {
					t.Fatal(err)
				}
				replaced = declared
			}
			replace()
			if _, err = Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit); err == nil {
				t.Fatal("a plan whose repository was replaced was executed")
			}
			for name, repository := range map[string]string{"the original": original, "the replacement": replaced} {
				if got := gitOutput(t, "", "--git-dir", repository, "rev-parse", "main"); got != before {
					t.Fatalf("%s repository moved from %s to %s", name, before, got)
				}
			}
		})
	}

	// Between the binding and the promotion of one publish.
	ctx, workspace, declared := developerPublishWorkspace(t)
	installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{nonce: true}})
	request := PublishRequest{Module: "payments", Environment: "production", Local: true, PromotionBranch: "codefly/promote-payments-production"}
	prepared, err := preparePublish(ctx, workspace, &request, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.cleanup()
	gitRun(t, prepared.dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "--quiet", "--allow-empty", "-m", "promotion")
	commit := gitOutput(t, prepared.dir, "rev-parse", "HEAD")
	before := gitOutput(t, "", "--git-dir", declared, "rev-parse", "main")
	swapped := filepath.Join(t.TempDir(), "swapped.git")
	gitRun(t, "", "clone", "--quiet", "--bare", declared, swapped)
	if err = os.Rename(declared, declared+".original"); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(swapped, declared); err != nil {
		t.Fatal(err)
	}
	if err = promoteLocally(ctx, prepared, commit); err == nil || !strings.Contains(err.Error(), "changed since the plan was bound") {
		t.Fatalf("a promotion into a repository swapped in after the binding was not refused: %v", err)
	}
	if got := gitOutput(t, "", "--git-dir", declared, "rev-parse", "main"); got != before {
		t.Fatalf("the swapped-in repository moved from %s to %s", before, got)
	}
}
