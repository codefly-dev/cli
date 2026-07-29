package gitops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/mutationauthority"
	"github.com/codefly-dev/core/resources"
)

var preparedPermit = mutationauthority.NewPreparedPermit()

func TestLocalGitopsPublishPlansThenCreatesSignedExactRefs(t *testing.T) {
	ctx := context.Background()
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	configureSSHSigning(t)

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true,
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID == "" || plan.Diff == "" || len(plan.Changed) == 0 {
		t.Fatalf("publication plan is not inspectable: %+v", plan)
	}
	if plan.Path != "environments/production/modules/payments" {
		t.Fatalf("publication path = %q", plan.Path)
	}
	if _, err := Publish(ctx, workspace, PublishMutation{Request: request, PlanID: plan.ID}, mutationauthority.PreparedPermit{}); err == nil || !strings.Contains(err.Error(), "prepared authority") {
		t.Fatalf("unprepared publication error = %v", err)
	}
	if _, err := Publish(ctx, workspace, PublishMutation{Request: request, PlanID: "sha256:stale"}, preparedPermit); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale plan error = %v", err)
	}
	result, err := Publish(ctx, workspace, PublishMutation{Request: request, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Signed || result.Commit == "" || result.Tree == "" {
		t.Fatalf("publication identities are incomplete: %+v", result)
	}
	if !strings.Contains(result.PullRequest, "#refs/codefly/reviews/") {
		t.Fatalf("local review ref = %q", result.PullRequest)
	}
	branch := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/"+request.PromotionBranch+"^{commit}")
	review := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/codefly/reviews/codefly-promote-payments-production^{commit}")
	if branch != result.Commit || review != result.Commit {
		t.Fatalf("published refs branch=%s review=%s, want %s", branch, review, result.Commit)
	}
	raw := gitOutput(t, "", "--git-dir", remote, "cat-file", "-p", result.Commit)
	if !strings.Contains(raw, "\ngpgsig ") {
		t.Fatalf("commit %s has no signature", result.Commit)
	}
	receipt, err := LoadPublishResult(workspace.Dir(), "payments", "production")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Commit != result.Commit || receipt.Tree != result.Tree {
		t.Fatalf("receipt = %+v, publication = %+v", receipt, result)
	}
}

func TestPublishRetriesPRAndReceiptForExistingSignedBranchCommit(t *testing.T) {
	ctx := context.Background()
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	configureSSHSigning(t)
	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true,
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, request)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Publish(ctx, workspace, PublishMutation{Request: request, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workspace.Dir(), ".codefly", "gitops", "publications", "payments-production.json")); err != nil {
		t.Fatal(err)
	}
	retryPlan, err := PlanPublish(ctx, workspace, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(retryPlan.Changed) != 0 || retryPlan.ExistingCommit != first.Commit {
		t.Fatalf("retry plan = %+v", retryPlan)
	}
	retried, err := Publish(ctx, workspace, PublishMutation{Request: request, PlanID: retryPlan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Commit != first.Commit || retried.Tree != first.Tree || retried.PullRequest != first.PullRequest {
		t.Fatalf("retried publication = %+v, first = %+v", retried, first)
	}
	if _, err := LoadPublishResult(workspace.Dir(), "payments", "production"); err != nil {
		t.Fatalf("retry did not restore publication receipt: %v", err)
	}
}

func TestPublishRejectsUnrelatedExistingPromotionChanges(t *testing.T) {
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")

	work := t.TempDir()
	gitRun(t, "", "clone", remote, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	gitRun(t, work, "checkout", "-b", "codefly/promote-payments-production")
	if err := os.WriteFile(filepath.Join(work, "unrelated.txt"), []byte("outside promotion\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "unrelated.txt")
	gitRun(t, work, "commit", "-m", "unrelated")
	gitRun(t, work, "push", "origin", "codefly/promote-payments-production")

	_, err := PlanPublish(context.Background(), workspace, PublishRequest{
		Module: "payments", Environment: "production", Local: true,
		PromotionBranch: "codefly/promote-payments-production",
	})
	if err == nil || !strings.Contains(err.Error(), "unrelated change") {
		t.Fatalf("plan error = %v", err)
	}
}

func TestRollbackRePromotesPriorReviewedTree(t *testing.T) {
	ctx := context.Background()
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	configureSSHSigning(t)
	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true,
		PromotionBranch: "codefly/promote-payments-production",
	}

	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	firstPlan, err := PlanPublish(ctx, workspace, request)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Publish(ctx, workspace, PublishMutation{Request: request, PlanID: firstPlan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	mergePromotionToMain(t, remote, request.PromotionBranch)

	renderPublishFixture(t, workspace.Dir(), "payments", "production", "worker")
	secondPlan, err := PlanPublish(ctx, workspace, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Publish(ctx, workspace, PublishMutation{Request: request, PlanID: secondPlan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if second.RenderDigest == first.RenderDigest {
		t.Fatal("second promotion did not change the rendered tree")
	}
	mergePromotionToMain(t, remote, request.PromotionBranch)
	if err := writeReceipt(workspace.Dir(), "evidence", "first.json", Evidence{
		SchemaVersion: SchemaVersion, Module: "payments", Environment: "production",
		RenderDigest: first.RenderDigest, SignedCommit: first.Commit, Tree: first.Tree,
		ArgoRevision: first.Commit, Cluster: "local-k3d", Health: "Healthy",
		Review: ReviewEvidence{
			URL: first.PullRequest, State: "LOCAL_REVIEW_REF",
			ReviewDecision: "LOCAL_QUALIFIED", MergeCommit: first.Commit,
		},
	}); err != nil {
		t.Fatal(err)
	}

	rollbackRequest := RollbackRequest{PublishRequest: request, ToRevision: first.Commit}
	rollbackPlan, err := PlanRollback(ctx, workspace, rollbackRequest)
	if err != nil {
		t.Fatal(err)
	}
	if rollbackPlan.RenderDigest != first.RenderDigest {
		t.Fatalf("rollback digest = %s, want %s", rollbackPlan.RenderDigest, first.RenderDigest)
	}
	rollback, err := Rollback(ctx, workspace, RollbackMutation{Request: rollbackRequest, PlanID: rollbackPlan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Commit == second.Commit || rollback.Tree == second.Tree {
		t.Fatalf("rollback did not create a new signed re-promotion: %+v", rollback)
	}
}

func TestRollbackRequiresEvidenceForSelectedModuleAndEnvironment(t *testing.T) {
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	revision := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/main")
	if err := writeReceipt(workspace.Dir(), "evidence", "other.json", Evidence{
		SchemaVersion: SchemaVersion, Module: "other", Environment: "production",
		SignedCommit: revision, ArgoRevision: revision, Health: "Healthy",
		Review: ReviewEvidence{
			State: "LOCAL_REVIEW_REF", ReviewDecision: "LOCAL_QUALIFIED",
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := PlanRollback(context.Background(), workspace, RollbackRequest{
		PublishRequest: PublishRequest{
			Module: "payments", Environment: "production", Local: true,
		},
		ToRevision: revision,
	})
	if err == nil || !strings.Contains(err.Error(), "no reviewed Healthy promotion evidence") {
		t.Fatalf("rollback evidence error = %v", err)
	}
}

func TestRemotePublishRequiresSafeGitHubRepository(t *testing.T) {
	tests := []string{
		"https://token@github.com/codefly-dev/manifests.git",
		"http://github.com/codefly-dev/manifests.git",
		"https://*.example.com/codefly-dev/manifests.git",
		"file:///tmp/manifests.git",
	}
	for _, repository := range tests {
		t.Run(repository, func(t *testing.T) {
			if _, err := validateRepositoryURL(repository, false); err == nil {
				t.Fatalf("unsafe repository %q accepted", repository)
			}
		})
	}
	for _, repository := range []string{
		"https://github.com/codefly-dev/manifests.git",
		"git@github.com:codefly-dev/manifests.git",
		"ssh://git@github.com/codefly-dev/manifests.git",
	} {
		t.Run(repository, func(t *testing.T) {
			slug, err := validateRepositoryURL(repository, false)
			if err != nil || slug != "codefly-dev/manifests" {
				t.Fatalf("safe repository %q => %q, %v", repository, slug, err)
			}
		})
	}
}

func mergePromotionToMain(t *testing.T, remote, branch string) {
	t.Helper()
	work := t.TempDir()
	gitRun(t, "", "clone", remote, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	gitRun(t, work, "merge", "--ff-only", "origin/"+branch)
	gitRun(t, work, "push", "origin", "main")
}

func createBareRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "manifests.git")
	gitRun(t, "", "init", "--bare", "--initial-branch=main", remote)
	work := filepath.Join(root, "seed")
	gitRun(t, "", "clone", remote, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("manifests\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "README.md")
	gitRun(t, work, "commit", "-m", "initial")
	gitRun(t, work, "push", "origin", "main")
	return remote
}

func loadGitopsWorkspace(t *testing.T, remote string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	config := fmt.Sprintf(`name: test
layout: flat
gitops:
  repo-url: file://%s
  path: environments
  branch: main
`, remote)
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func renderPublishFixture(t *testing.T, root, module, environment, name string) {
	t.Helper()
	destination := filepath.Join(root, "deployments", "environments", environment, "modules", module)
	_, err := RenderOwnedTree(context.Background(), RenderOptions{
		Destination: destination, Module: module, Environment: environment, Promotable: true,
	}, func(ctx context.Context, stage string) error {
		manifest := strings.Replace(pinnedDeployment, "name: api", "name: "+name, 2)
		return os.WriteFile(filepath.Join(stage, "deployment.yaml"), []byte(manifest), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func configureSSHSigning(t *testing.T) {
	t.Helper()
	key := filepath.Join(t.TempDir(), "signing-key")
	command := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, output)
	}
	t.Setenv("GIT_AUTHOR_NAME", "Codefly Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "codefly@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Codefly Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "codefly@example.com")
	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("GIT_CONFIG_KEY_0", "gpg.format")
	t.Setenv("GIT_CONFIG_VALUE_0", "ssh")
	t.Setenv("GIT_CONFIG_KEY_1", "user.signingKey")
	t.Setenv("GIT_CONFIG_VALUE_1", key)
	t.Setenv("GIT_CONFIG_KEY_2", "gpg.ssh.program")
	t.Setenv("GIT_CONFIG_VALUE_2", sshKeygen)
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = gitOutput(t, dir, args...)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
