package gitops

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/solutionhost/cell"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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
		Workspace: "payments", Host: env.Host, Composition: workspace, Target: env,
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

// cellWorkload finds a workload by name in a delivered cell's one namespace.
func cellWorkload(t *testing.T, file *cell.File, name string) *cell.Workload {
	t.Helper()
	for index := range file.Namespaces[0].Workloads {
		if file.Namespaces[0].Workloads[index].Name == name {
			return &file.Namespaces[0].Workloads[index]
		}
	}
	t.Fatalf("the delivered cell carries no workload %s", name)
	return nil
}

// TestRollbackRestoresTheDeclarationsTheRevisionWasRenderedWith: the cell's
// declarations — endpoints and their visibility among them — are the
// composition's as it was when the tree was RENDERED, recorded with the tree,
// not as it stands at publish. Publish A (the host's endpoint internal),
// change the declaration, render and publish B (public), roll back to A: the
// delivered cell says internal again, which a derivation over the composition
// as it stands now would not.
func TestRollbackRestoresTheDeclarationsTheRevisionWasRenderedWith(t *testing.T) {
	ctx, workspace, env, remote := hostedPublishWorkspace(t)
	writeTestCell(t, workspace, env, "production", "payments")
	request := hostedPublishRequest()
	first := publishHosted(t, ctx, workspace, &request)
	require.Equal(t, "internal", cellWorkload(t, deliveredCell(t, remote, request.PromotionBranch), "host").Endpoints[0].Visibility)
	mergePromotionToMain(t, remote, request.PromotionBranch)

	hostService := filepath.Join(workspace.Dir(), "services", "host", resources.ServiceConfigurationName)
	require.NoError(t, os.WriteFile(hostService, []byte(devServiceYAML("host")+"endpoints:\n  - name: rest\n    api: rest\n    visibility: public\n"), 0o644))
	changed, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	changedEnv := selectedEnvironment(t, changed, "production")
	renderHostedPayments(t, changed, changedEnv, pinnedDeployment, hostedPaymentsInstances())
	writeTestCell(t, changed, changedEnv, "production", "payments")
	publishHosted(t, ctx, changed, &request)
	require.Equal(t, "public", cellWorkload(t, deliveredCell(t, remote, request.PromotionBranch), "host").Endpoints[0].Visibility)
	mergePromotionToMain(t, remote, request.PromotionBranch)

	require.NoError(t, writeReceipt(changed.Dir(), "evidence", "first.json", Evidence{
		SchemaVersion: EvidenceSchemaVersion, Module: "payments", Environment: "production",
		RenderDigest: first.RenderDigest, SignedCommit: first.Commit, Tree: first.Tree,
		ArgoRevision: first.Commit, Cluster: "local-k3d", Health: "Healthy",
		Review: ReviewEvidence{URL: first.PullRequest, State: "LOCAL_REVIEW_REF", ReviewDecision: "LOCAL_QUALIFIED", MergeCommit: first.Commit},
	}))
	rollbackRequest := RollbackRequest{PublishRequest: request, ToRevision: first.Commit}
	rollbackPlan, err := PlanRollback(ctx, changed, &rollbackRequest)
	require.NoError(t, err)
	_, err = Rollback(ctx, changed, &RollbackMutation{Request: rollbackRequest, PlanID: rollbackPlan.ID, Carriers: rollbackPlan.Carriers}, preparedPermit)
	require.NoError(t, err)
	require.Equal(t, "internal", cellWorkload(t, deliveredCell(t, remote, request.PromotionBranch), "host").Endpoints[0].Visibility, "the rolled-back cell carries the declaration A was rendered with")
}

// TestPublishRefusesAMergedCellCoreRejects: a delivered cell and a
// contribution each valid on their own can merge into a cell core refuses —
// one namespace name claimed by two modules — and the publish refuses it
// before a byte is staged, rather than delivering a file the next publisher
// cannot read.
func TestPublishRefusesAMergedCellCoreRejects(t *testing.T) {
	repository := newDeliveryRepository(t)
	cellPath := "deployments/cells/prod/cell.yaml"
	entry := func(module string) cell.Namespace {
		return cell.Namespace{Name: "payments", Module: module, Workloads: []cell.Workload{{
			Name: module, Kind: "Deployment", Selector: map[string]string{"app": module}, Service: module + "/api", ServiceAccount: "api",
			SPIFFEID: "spiffe://cluster.example/ns/payments/sa/api", Authenticating: "api",
			Containers: []cell.Container{{Name: "api", Image: cell.Image{Repository: "registry.example.test/" + module, Digest: "sha256:" + strings.Repeat("a", 64)}}},
			Artifact:   cell.Artifact{Name: "api", Digest: "sha256:" + strings.Repeat("a", 64)},
		}}}
	}
	header := cell.File{Schema: cell.SchemaV1, Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example", TrustDomain: "cluster.example", Environment: "prod"}
	delivered := header
	delivered.Namespaces = []cell.Namespace{entry("crm")}
	require.NoError(t, delivered.Validate(), "the delivered cell is valid on its own")
	deliveredBytes := commitDeliveredCell(t, repository.repo, cellPath, &delivered, nil)
	contribution := header
	contribution.Namespaces = []cell.Namespace{entry("payments")}
	require.NoError(t, contribution.Validate(), "the contribution is valid on its own")
	publication := &deliveryPublication{baseBranch: "main", cellPath: cellPath,
		options: deliveryPublishOptions{Module: "payments", Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example", TrustDomain: "cluster.example"}}
	err := stageCellContribution(context.Background(), repository.repo, publication, &contribution, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not validate")
	staged, err := os.ReadFile(filepath.Join(repository.repo, filepath.FromSlash(cellPath)))
	require.NoError(t, err)
	require.Equal(t, string(deliveredBytes), string(staged), "nothing was staged over the delivered cell")
	_, err = os.Stat(filepath.Join(repository.repo, "deployments", "cells", "prod", consumersLedgerFile))
	require.True(t, os.IsNotExist(err), "no ledger was staged either")
}

// commitDeliveredCell writes a cell (and a consumers ledger, when given) to
// the delivery repository's base branch, as a publish before this one would
// have, and returns the cell's bytes.
func commitDeliveredCell(t *testing.T, repo, cellPath string, file *cell.File, ledger consumersLedger) []byte {
	t.Helper()
	data, err := yaml.Marshal(file)
	require.NoError(t, err)
	full := filepath.Join(repo, filepath.FromSlash(cellPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, data, 0o600))
	if ledger != nil {
		encoded, err := json.MarshalIndent(ledger, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(full), consumersLedgerFile), append(encoded, '\n'), 0o600))
	}
	for _, args := range [][]string{{"add", "-A", "--", filepath.ToSlash(filepath.Dir(cellPath))}, {"commit", "-q", "--allow-empty", "-m", "cell"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, "git %v: %s", args, out)
	}
	return data
}

// TestCellMergeCarriesAnEdgeInEitherPublicationOrder: a consumer edge is the
// consumer's declaration, recorded in the consumers ledger by the consumer's
// publish; the delivered cell's consumer lists are derived from the ledger,
// so the edge reaches the provider's entry whether the consumer or the
// provider publishes first, and leaves when the consumer stops declaring it
// — never when the provider publishes.
func TestCellMergeCarriesAnEdgeInEitherPublicationOrder(t *testing.T) {
	ctx := context.Background()
	cellPath := "deployments/cells/prod/cell.yaml"
	header := func(namespaces ...cell.Namespace) *cell.File {
		return &cell.File{Schema: cell.SchemaV1, Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example", TrustDomain: "cluster.example", Environment: "prod", Namespaces: namespaces}
	}
	billing := func() cell.Namespace {
		return cell.Namespace{Name: "ns-billing", Module: "billing", Workloads: []cell.Workload{{
			Name: "billing", Kind: "Deployment", Selector: map[string]string{"app": "billing"}, Service: "billing/api", ServiceAccount: "api",
			SPIFFEID: "spiffe://cluster.example/ns/ns-billing/sa/api", Authenticating: "api",
			Containers: []cell.Container{{Name: "api", Image: cell.Image{Repository: "registry.example.test/billing", Digest: "sha256:" + strings.Repeat("d", 64)}}},
			Artifact:   cell.Artifact{Name: "api", Digest: "sha256:" + strings.Repeat("d", 64)},
			Endpoints:  []cell.Endpoint{{Name: "grpc"}},
		}}}
	}
	crm := cell.Namespace{Name: "ns-crm", Module: "crm"}
	edge := []ConsumedEndpoint{{Provider: "billing/api", Endpoint: "grpc", Consumer: "crm/api"}}
	consumersOf := func(file *cell.File) []string {
		for _, namespace := range file.Namespaces {
			if namespace.Module == "billing" {
				return namespace.Workloads[0].Endpoints[0].Consumers
			}
		}
		return nil
	}

	// The consumer first: its edge waits in the ledger for the provider.
	repository := newDeliveryRepository(t)
	merged, ledger, err := mergeCellContribution(ctx, repository.repo, "main", cellPath, header(crm), "crm", edge)
	require.NoError(t, err)
	require.Equal(t, consumersLedger{"crm": edge}, ledger)
	commitDeliveredCell(t, repository.repo, cellPath, merged, ledger)
	merged, _, err = mergeCellContribution(ctx, repository.repo, "main", cellPath, header(billing()), "billing", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"crm/api"}, consumersOf(merged), "the provider's first publish carries the edge the consumer recorded")

	// The provider first: the consumer's publish writes the edge into the
	// provider's delivered entry.
	repository = newDeliveryRepository(t)
	merged, ledger, err = mergeCellContribution(ctx, repository.repo, "main", cellPath, header(billing()), "billing", nil)
	require.NoError(t, err)
	require.Empty(t, consumersOf(merged))
	commitDeliveredCell(t, repository.repo, cellPath, merged, ledger)
	merged, ledger, err = mergeCellContribution(ctx, repository.repo, "main", cellPath, header(crm), "crm", edge)
	require.NoError(t, err)
	require.Equal(t, []string{"crm/api"}, consumersOf(merged))
	commitDeliveredCell(t, repository.repo, cellPath, merged, ledger)

	// The provider publishes again: the edge is the consumer's, so it stays.
	merged, ledger, err = mergeCellContribution(ctx, repository.repo, "main", cellPath, header(billing()), "billing", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"crm/api"}, consumersOf(merged))
	commitDeliveredCell(t, repository.repo, cellPath, merged, ledger)

	// The consumer stops declaring it: its publish removes it.
	merged, ledger, err = mergeCellContribution(ctx, repository.repo, "main", cellPath, header(crm), "crm", nil)
	require.NoError(t, err)
	require.Empty(t, consumersOf(merged))
	require.NotContains(t, ledger, "crm")
}

// TestPackagedSolutionRollbackContributesTheRestoredRecordsCell: a packaged
// solution's cell entry is hand-written and recorded with its render, so a
// rollback restores the entry the revision was rendered with — publish A
// (egress to a.example), publish B (b.example), roll back: a.example again —
// instead of silently keeping B's cell under A's workloads.
func TestPackagedSolutionRollbackContributesTheRestoredRecordsCell(t *testing.T) {
	ctx := context.Background()
	installFakeSolutionExecutor(t, &fakeSolutionExecutor{})
	remote := createBareRepository(t)
	workspace := loadSolutionWorkspace(t, remote)
	env := selectedEnvironment(t, workspace, "local")
	require.NotNil(t, env)
	agent := &resources.Agent{Kind: resources.SolutionAgent, Publisher: "codefly.dev", Name: "hello-solution", Version: "0.0.1"}
	render := func(egressHost string) {
		t.Helper()
		writeHandCellWithEgress(t, workspace, env, "local", "lastlogin-go", egressHost)
		_, err := RenderSolution(ctx, &SolutionRenderRequest{
			Workspace: workspace, Environment: env, Agent: agent, Name: "lastlogin-go",
			Source:     filepath.Join(workspace.Dir(), "solution-src"),
			Reference:  "ghcr.io/codefly-dev/hello-solution:0.0.1",
			AppProject: "lastlogin-go",
		})
		require.NoError(t, err)
	}
	egressOf := func(file *cell.File) string {
		require.Len(t, file.Namespaces[0].Egress, 1)
		return file.Namespaces[0].Egress[0].Hosts[0].Name
	}
	configureSSHSigning(t)
	request := PublishRequest{Module: "lastlogin-go", Environment: "local", Local: true, PromotionBranch: "codefly/promote-lastlogin-go-local"}
	cellOnBranch := func() *cell.File {
		out, err := exec.Command("git", "--git-dir", remote, "show", "refs/heads/"+request.PromotionBranch+":environments/cells/local/cell.yaml").Output()
		require.NoError(t, err)
		file, err := cell.Parse(out)
		require.NoError(t, err)
		return file
	}

	render("a.example")
	first := publishHosted(t, ctx, workspace, &request)
	require.Equal(t, "a.example", egressOf(cellOnBranch()))
	mergePromotionToMain(t, remote, request.PromotionBranch)
	render("b.example")
	second := publishHosted(t, ctx, workspace, &request)
	require.NotEqual(t, first.RenderDigest, second.RenderDigest)
	require.Equal(t, "b.example", egressOf(cellOnBranch()))
	mergePromotionToMain(t, remote, request.PromotionBranch)

	require.NoError(t, writeReceipt(workspace.Dir(), "evidence", "first.json", Evidence{
		SchemaVersion: EvidenceSchemaVersion, Module: "lastlogin-go", Environment: "local",
		RenderDigest: first.RenderDigest, SignedCommit: first.Commit, Tree: first.Tree,
		ArgoRevision: first.Commit, Cluster: "local-k3d", Health: "Healthy",
		Review: ReviewEvidence{URL: first.PullRequest, State: "LOCAL_REVIEW_REF", ReviewDecision: "LOCAL_QUALIFIED", MergeCommit: first.Commit},
	}))
	rollbackRequest := RollbackRequest{PublishRequest: request, ToRevision: first.Commit}
	rollbackPlan, err := PlanRollback(ctx, workspace, &rollbackRequest)
	require.NoError(t, err)
	_, err = Rollback(ctx, workspace, &RollbackMutation{Request: rollbackRequest, PlanID: rollbackPlan.ID, Carriers: rollbackPlan.Carriers}, preparedPermit)
	require.NoError(t, err)
	require.Equal(t, "a.example", egressOf(cellOnBranch()), "the rolled-back cell is the entry A was rendered with")
}

// writeHandCellWithEgress is writeHandCell with one egress host, so two
// renders of a packaged solution record different entries.
func writeHandCellWithEgress(t *testing.T, workspace *resources.Workspace, env *environments.Environment, environment, module, egressHost string) {
	t.Helper()
	cellFile := cell.File{
		Schema: cell.SchemaV1, Coordinate: env.Host.Coordinate, Component: env.Host.Component,
		Domain: env.Host.Domain, TrustDomain: env.Host.TrustDomain, Environment: environment,
		Namespaces: []cell.Namespace{{Name: module, Module: module, Egress: []cell.Egress{{Service: module + "/app", Hosts: []cell.EgressHost{{Name: egressHost, Port: 443}}}}}},
	}
	require.NoError(t, cellFile.Validate())
	data, err := yaml.Marshal(&cellFile)
	require.NoError(t, err)
	path := cellPath(workspace.Dir(), environment)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
