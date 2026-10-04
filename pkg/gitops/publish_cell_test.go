package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/solutionhost/cell"
)

// hostedPaymentsInstances is the presence the hosted fixture declares.
func hostedPaymentsInstances() []SolutionInstance {
	return []SolutionInstance{{
		Kind: solutionhost.KindModule, Name: "payments", Package: "example/payments", Version: "1.0.0", ReleaseDigest: testReleaseDigest,
		Units: []SolutionArtifactUnit{
			{Name: "api", Path: "services/api", Subject: "payments@example.iam.test"},
			{Name: "host", Path: "services/host", Subject: "payments@example.iam.test"},
		},
	}}
}

// renderHostedPayments renders the payments module of the hosted fixture with
// the given Deployment manifest for its two services and the presence it
// declares; nil declares none, which withdraws it.
func renderHostedPayments(t *testing.T, workspace *resources.Workspace, env *environments.Environment, deployment string, instances []SolutionInstance) {
	t.Helper()
	destination := filepath.Join(workspace.Dir(), "deployments", "modules", "payments")
	if _, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: destination, Module: "payments", UnitNames: []string{"api", "host"},
		OwnedPath:   filepath.ToSlash(filepath.Join("environments", "deployments", "modules", "payments")),
		Units:       promotableServiceGraph("payments", []string{"api", "host"}),
		Environment: "production", Namespace: "payments", AppProject: "payments", Promotable: true,
		Workspace: "payments", Host: env.Host,
		SolutionInstances: instances,
	}, func(_ context.Context, stage string) error {
		for _, name := range []string{"api", "host"} {
			overlay := filepath.Join(stage, "services", name, "overlays", "production")
			if err := os.MkdirAll(overlay, 0o755); err != nil {
				return err
			}
			manifest := strings.ReplaceAll(deployment, "name: api", "name: "+name)
			if err := os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(manifest), 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// deliveredCell reads the cell file a promotion branch carries on the bare
// remote, as the platform would read it.
func deliveredCell(t *testing.T, remote, branch string) *cell.File {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", remote, "show", "refs/heads/"+branch+":environments/cells/production/cell.yaml").Output()
	if err != nil {
		t.Fatalf("read the delivered cell on %s: %v", branch, err)
	}
	file, err := cell.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func hostedPublishRequest() PublishRequest {
	return PublishRequest{Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{}, PromotionBranch: "codefly/promote-payments-production"}
}

func publishHosted(t *testing.T, ctx context.Context, workspace *resources.Workspace, request *PublishRequest) PublishResult {
	t.Helper()
	plan, err := PlanPublish(ctx, workspace, request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: *request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestRollbackContributesTheRestoredTreesCell: publish A, publish B with
// another image, roll back to A — the delivered cell describes A again. The
// rollback's cell staging was a no-op for one round, because the cell's
// destination was set only while loading a forward render.
func TestRollbackContributesTheRestoredTreesCell(t *testing.T) {
	ctx, workspace, env, remote := hostedPublishWorkspace(t)
	writeTestCell(t, workspace, env, "production", "payments")
	request := hostedPublishRequest()
	first := publishHosted(t, ctx, workspace, &request)
	digestA := "sha256:" + strings.Repeat("a", 64)
	if got := deliveredCell(t, remote, request.PromotionBranch).Namespaces[0].Workloads[0].Containers[0].Image.Digest; got != digestA {
		t.Fatalf("the first publish's cell runs %s, want A", got)
	}
	mergePromotionToMain(t, remote, request.PromotionBranch)

	digestB := "sha256:" + strings.Repeat("b", 64)
	renderHostedPayments(t, workspace, env, strings.ReplaceAll(pinnedDeployment, digestA, digestB), hostedPaymentsInstances())
	writeTestCell(t, workspace, env, "production", "payments")
	second := publishHosted(t, ctx, workspace, &request)
	if got := deliveredCell(t, remote, request.PromotionBranch).Namespaces[0].Workloads[0].Containers[0].Image.Digest; got != digestB {
		t.Fatalf("the second publish's cell runs %s, want B", got)
	}
	if second.RenderDigest == first.RenderDigest {
		t.Fatal("the second publish did not change the rendered tree")
	}
	mergePromotionToMain(t, remote, request.PromotionBranch)

	if err := writeReceipt(workspace.Dir(), "evidence", "first.json", Evidence{
		SchemaVersion: EvidenceSchemaVersion, Module: "payments", Environment: "production",
		RenderDigest: first.RenderDigest, SignedCommit: first.Commit, Tree: first.Tree,
		ArgoRevision: first.Commit, Cluster: "local-k3d", Health: "Healthy",
		Review: ReviewEvidence{URL: first.PullRequest, State: "LOCAL_REVIEW_REF", ReviewDecision: "LOCAL_QUALIFIED", MergeCommit: first.Commit},
	}); err != nil {
		t.Fatal(err)
	}
	rollbackRequest := RollbackRequest{PublishRequest: request, ToRevision: first.Commit}
	rollbackPlan, err := PlanRollback(ctx, workspace, &rollbackRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Rollback(ctx, workspace, &RollbackMutation{Request: rollbackRequest, PlanID: rollbackPlan.ID, Carriers: rollbackPlan.Carriers}, preparedPermit); err != nil {
		t.Fatal(err)
	}
	restored := deliveredCell(t, remote, request.PromotionBranch)
	if got := restored.Namespaces[0].Workloads[0].Containers[0].Image.Digest; got != digestA {
		t.Fatalf("after the rollback the delivered cell runs %s, want A", got)
	}
	if restored.Namespaces[0].Delivery == nil {
		t.Fatal("the rolled-back cell declares no delivery Job")
	}
}

// TestPublishWithdrawsTheLastPresenceAndStillPublishesItsCell: a render that
// declares no presence any more has no delivery in its cell — the render
// cannot know the tombstones the publish synthesizes — while the publish
// delivers them and must describe their Job. The render's cell is held to the
// render's inventory, and the delivered cell is derived from the settled one;
// publishing again with only the retained tombstones goes the same way.
func TestPublishWithdrawsTheLastPresenceAndStillPublishesItsCell(t *testing.T) {
	ctx, workspace, env, remote := hostedPublishWorkspace(t)
	writeTestCell(t, workspace, env, "production", "payments")
	request := hostedPublishRequest()
	publishHosted(t, ctx, workspace, &request)
	mergePromotionToMain(t, remote, request.PromotionBranch)

	// The last presence declaration is withdrawn: the render declares none.
	renderHostedPayments(t, workspace, env, pinnedDeployment, nil)
	writeTestCell(t, workspace, env, "production", "payments")
	for round := 1; round <= 2; round++ {
		plan, err := PlanPublish(ctx, workspace, &request)
		if err != nil {
			t.Fatalf("round %d: the withdrawal was refused: %v", round, err)
		}
		if plan.Delivery == nil || len(plan.Delivery.Documents) != 1 || !plan.Delivery.Documents[0].Removed {
			t.Fatalf("round %d: the plan does not deliver the tombstone: %+v", round, plan.Delivery)
		}
		if _, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if deliveredCell(t, remote, request.PromotionBranch).Namespaces[0].Delivery == nil {
			t.Fatalf("round %d: the delivered cell declares no Job for the tombstone it delivers", round)
		}
		mergePromotionToMain(t, remote, request.PromotionBranch)
	}
}
