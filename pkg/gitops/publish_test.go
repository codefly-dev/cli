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
	"github.com/codefly-dev/cli/pkg/orchestration"
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
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID == "" || plan.Diff == "" || len(plan.Changed) == 0 {
		t.Fatalf("publication plan is not inspectable: %+v", plan)
	}
	if plan.Path != "environments/deployments/modules/payments" {
		t.Fatalf("publication path = %q", plan.Path)
	}
	snapshotRef := "refs/heads/codefly/snapshot-payments-production"
	if err := exec.Command("git", "--git-dir", remote, "show-ref", "--verify", snapshotRef).Run(); err == nil {
		t.Fatal("publication plan advertised the service snapshot")
	}
	if _, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID}, mutationauthority.PreparedPermit{}); err == nil || !strings.Contains(err.Error(), "prepared authority") {
		t.Fatalf("unprepared publication error = %v", err)
	}
	if _, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: "sha256:stale"}, preparedPermit); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale plan error = %v", err)
	}
	if err := exec.Command("git", "--git-dir", remote, "show-ref", "--verify", snapshotRef).Run(); err == nil {
		t.Fatal("stale publication advertised the service snapshot")
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Signed || result.SnapshotRevision == "" || result.Commit == "" || result.Tree == "" {
		t.Fatalf("publication identities are incomplete: %+v", result)
	}
	if result.SnapshotRevision == result.Commit {
		t.Fatalf("service snapshot and signed publication commit are not distinct: %+v", result)
	}
	if !strings.Contains(result.PullRequest, "#refs/codefly/reviews/") {
		t.Fatalf("local review ref = %q", result.PullRequest)
	}
	branch := gitOutput(t, "", "--git-dir", remote, "rev-parse", "refs/heads/"+request.PromotionBranch+"^{commit}")
	review := gitOutput(t, "", "--git-dir", remote, "rev-parse", localReviewRef(request.PromotionBranch, result.Commit)+"^{commit}")
	if branch != result.Commit || review != result.Commit {
		t.Fatalf("published refs branch=%s review=%s, want %s", branch, review, result.Commit)
	}
	snapshot := gitOutput(t, "", "--git-dir", remote, "rev-parse", snapshotRef+"^{commit}")
	if snapshot != result.SnapshotRevision {
		t.Fatalf("published snapshot ref = %s, want %s", snapshot, result.SnapshotRevision)
	}
	gitRun(t, "", "--git-dir", remote, "merge-base", "--is-ancestor", result.SnapshotRevision, result.Commit)
	serviceInventory := gitOutput(
		t,
		"",
		"--git-dir",
		remote,
		"show",
		result.SnapshotRevision+":"+result.Path+"/"+InventoryFilename,
	)
	if !strings.Contains(serviceInventory, `"serviceGraph": [`) || !strings.Contains(serviceInventory, `"service": "api"`) {
		t.Fatalf("immutable service inventory = %s", serviceInventory)
	}
	raw := gitOutput(t, "", "--git-dir", remote, "cat-file", "-p", result.Commit)
	if !strings.Contains(raw, "\ngpgsig ") {
		t.Fatalf("commit %s has no signature", result.Commit)
	}
	receipt, err := LoadPublishResult(workspace.Dir(), "payments", "production")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SnapshotRevision != result.SnapshotRevision || receipt.Commit != result.Commit || receipt.Tree != result.Tree {
		t.Fatalf("receipt = %+v, publication = %+v", receipt, result)
	}
}

func TestBootstrapApplicationsRequireTheImmutableServiceSnapshot(t *testing.T) {
	root := t.TempDir()
	application := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: payments-api
spec:
  source:
    targetRevision: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`
	if err := os.WriteFile(filepath.Join(root, "application.yaml"), []byte(application), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateBootstrapRevision(root, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	err := validateBootstrapRevision(root, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err == nil || !strings.Contains(err.Error(), "expected service snapshot") {
		t.Fatalf("snapshot mismatch error = %v", err)
	}
}

func TestArgoRepositoryUsesWorkspaceFetchURLForLocalPromotion(t *testing.T) {
	workspace := loadGitopsWorkspace(t, createBareRepository(t))
	config, _, _, _, err := resolveGitops(workspace, "production", true)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := argoRepository(config)
	if err != nil {
		t.Fatal(err)
	}
	if repository != "https://host.k3d.internal/manifests.git" {
		t.Fatalf("Argo repository = %q", repository)
	}

	config.FetchRepoURL = ""
	if _, err := argoRepository(config); err == nil || !strings.Contains(err.Error(), "fetch-repo-url") {
		t.Fatalf("missing local fetch repository error = %v", err)
	}
}

func TestArgoRepositoryRejectsProductionFetchRepositoryMismatch(t *testing.T) {
	config := &repositoryConfig{
		RepoURL:      "git@github.com:codefly-dev/manifests.git",
		FetchRepoURL: "https://github.com/codefly-dev/other-manifests.git",
	}
	if _, err := argoRepository(config); err == nil ||
		!strings.Contains(err.Error(), "must identify the publication repository") {
		t.Fatalf("mismatched fetch repository error = %v", err)
	}
	config.FetchRepoURL = "https://github.com/codefly-dev/manifests.git"
	repository, err := argoRepository(config)
	if err != nil {
		t.Fatal(err)
	}
	if repository != config.FetchRepoURL {
		t.Fatalf("Argo repository = %q", repository)
	}
}

func TestModuleBundleRejectsArgoTransportResources(t *testing.T) {
	root := t.TempDir()
	application := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: agent-owned
`
	if err := os.WriteFile(filepath.Join(root, "application.yaml"), []byte(application), 0o644); err != nil {
		t.Fatal(err)
	}
	err := validateTransportNeutralModuleBundle(root)
	if err == nil || !strings.Contains(err.Error(), "CLI-owned Argo transport resource Application") {
		t.Fatalf("Argo transport resource error = %v", err)
	}
}

func TestManagedServicePromotionRetainsOnlyItsBootstrapJob(t *testing.T) {
	root := t.TempDir()
	serviceRoot := filepath.Join(root, "services", "store")
	if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	rendered := `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
---
apiVersion: v1
kind: Service
metadata:
  name: store
---
apiVersion: batch/v1
kind: Job
metadata:
  name: store-migrate-aaaaaaaaaaaa
  labels:
    codefly.dev/bootstrap-service: store
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: ghcr.io/codefly-dev/store-migrate@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`
	if err := os.WriteFile(filepath.Join(serviceRoot, "rendered.yaml"), []byte(rendered), 0o644); err != nil {
		t.Fatal(err)
	}
	retained, err := retainManagedBootstrap(serviceRoot, "store", "production")
	if err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("managed migration Job was not retained")
	}
	var retainedManifests string
	err = walkRegularFiles(serviceRoot, func(path, _ string, _ os.FileInfo) error {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		retainedManifests += string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(retainedManifests, "kind: Job") ||
		strings.Contains(retainedManifests, "kind: StatefulSet") ||
		strings.Contains(retainedManifests, "kind: Service\n") {
		t.Fatalf("managed bootstrap tree = %s", retainedManifests)
	}

	revision := strings.Repeat("a", 40)
	inventory := &Inventory{
		SchemaVersion: SchemaVersion,
		Module:        "payments",
		Environment:   "production",
		Namespace:     "payments",
		AppProject:    "payments-production",
		ServiceGraph: []InventoryService{{
			Module: "payments", Service: "store", Path: "services/store",
			Managed: true, Bootstrap: true,
		}},
	}
	config := &repositoryConfig{RepoURL: "https://github.com/codefly-dev/manifests.git"}
	if err := generateArgoBootstrap(
		context.Background(),
		config,
		root,
		"environments/deployments/modules/payments",
		inventory,
		"production",
		revision,
	); err != nil {
		t.Fatal(err)
	}
	application, err := os.ReadFile(filepath.Join(root, "bootstrap", "applications", "payments-store.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(application), "targetRevision: "+revision) ||
		!strings.Contains(string(application), "path: environments/deployments/modules/payments/services/store/overlays/production") {
		t.Fatalf("managed bootstrap Application = %s", application)
	}
}

func TestPublishComposesTransportNeutralModuleBundleIntoArgoPromotion(t *testing.T) {
	ctx := context.Background()
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspaceWithAgent(t, remote)
	configureSSHSigning(t)
	t.Setenv("GITHUB_TOKEN", "must-not-reach-module-agent")
	t.Setenv("AWS_ACCESS_KEY_ID", "must-not-reach-module-agent")
	t.Setenv("KUBECONFIG", "/must/not/reach/module-agent")
	t.Setenv("PULUMI_ACCESS_TOKEN", "must-not-reach-module-agent")
	t.Setenv("SSH_AUTH_SOCK", "/must/not/reach/module-agent.sock")

	home := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, home)
	agent := &resources.Agent{
		Kind: resources.ModuleAgent, Publisher: "codefly.dev", Name: "gitops-test", Version: "1.0.0",
	}
	binary, err := agent.Path(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	generator := `#!/bin/sh
set -eu
if grep -Eq 'gitops:|repo-url:|fetch-repo-url:|branch:' "$PWD/workspace.codefly.yaml"; then
  echo "module agent received GitOps authority" >&2
  exit 90
fi
if [ -n "${GITHUB_TOKEN-}${AWS_ACCESS_KEY_ID-}${KUBECONFIG-}${PULUMI_ACCESS_TOKEN-}${GIT_CONFIG_COUNT-}${SSH_AUTH_SOCK-}${CODEFLY_HOME-}" ]; then
  echo "module agent inherited host credentials or configuration" >&2
  exit 91
fi
module_dir="$1"
destination="$module_dir/deployment/kustomize"
mkdir -p "$destination/overlays/production/resources"
cat > "$destination/overlays/production/resources/namespace.yaml" <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: payments
EOF
cat > "$destination/overlays/production/resources/external-secret.yaml" <<'EOF'
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: payments-store
  namespace: payments
spec:
  secretStoreRef:
    kind: ClusterSecretStore
    name: production
  target:
    name: payments-store
  data:
    - secretKey: connection
      remoteRef:
        key: payments/store
        property: connection
EOF
cat > "$destination/overlays/production/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - resources/namespace.yaml
  - resources/external-secret.yaml
EOF
cat > "$destination/bundle.json" <<'EOF'
{
  "schemaVersion": "codefly.dev/module-bundle/v1",
  "module": "payments",
  "namespace": "payments",
  "serviceEntry": "api",
  "environments": [{
    "name": "production",
    "namespace": "payments",
    "cluster": "k3d",
    "resourcePath": "overlays/production",
    "services": ["api"],
    "ingress": []
  }]
}
EOF
`
	if err := os.WriteFile(binary, []byte(generator), 0o755); err != nil {
		t.Fatal(err)
	}
	renderPublishFixtureWithModuleAgent(t, workspace, "payments", "production", "api")

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true,
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	moduleResource := gitOutput(
		t,
		"",
		"--git-dir",
		remote,
		"show",
		result.SnapshotRevision+":"+result.Path+"/module/overlays/production/resources/namespace.yaml",
	)
	if !strings.Contains(moduleResource, "name: payments") {
		t.Fatalf("module resource snapshot = %s", moduleResource)
	}
	externalSecret := gitOutput(
		t,
		"",
		"--git-dir",
		remote,
		"show",
		result.SnapshotRevision+":"+result.Path+"/module/overlays/production/resources/external-secret.yaml",
	)
	if !strings.Contains(externalSecret, "kind: ExternalSecret") ||
		!strings.Contains(externalSecret, "key: payments/store") {
		t.Fatalf("external secret snapshot = %s", externalSecret)
	}
	project := gitOutput(
		t,
		"",
		"--git-dir",
		remote,
		"show",
		result.Commit+":"+result.Path+"/bootstrap/project.yaml",
	)
	if strings.Contains(project, `group: "*"`) || strings.Contains(project, `kind: "*"`) ||
		!strings.Contains(project, "repoURL") && !strings.Contains(project, "sourceRepos") ||
		!strings.Contains(project, "group: external-secrets.io") ||
		!strings.Contains(project, "kind: ExternalSecret") {
		t.Fatalf("generated AppProject authority = %s", project)
	}
	application := gitOutput(
		t,
		"",
		"--git-dir",
		remote,
		"show",
		result.Commit+":"+result.Path+"/bootstrap/applications/payments-api.yaml",
	)
	if !strings.Contains(application, "targetRevision: "+result.SnapshotRevision) ||
		!strings.Contains(application, "path: environments/deployments/modules/payments/services/api/overlays/production") {
		t.Fatalf("generated Application = %s", application)
	}
	moduleApplication := gitOutput(
		t,
		"",
		"--git-dir",
		remote,
		"show",
		result.Commit+":"+result.Path+"/bootstrap/applications/payments-resources.yaml",
	)
	if !strings.Contains(moduleApplication, "targetRevision: "+result.SnapshotRevision) ||
		!strings.Contains(moduleApplication, "path: environments/deployments/modules/payments/module/overlays/production") {
		t.Fatalf("generated module Application = %s", moduleApplication)
	}
	bootstrap := gitOutput(
		t,
		"",
		"--git-dir",
		remote,
		"show",
		result.Commit+":"+result.Path+"/bootstrap/kustomization.yaml",
	)
	if !strings.Contains(bootstrap, "project.yaml") ||
		!strings.Contains(bootstrap, "applications/payments-resources.yaml") ||
		!strings.Contains(bootstrap, "applications/payments-api.yaml") {
		t.Fatalf("generated Argo bootstrap = %s", bootstrap)
	}
}

func TestPlanPublishRejectsModuleBundleWithPhantomService(t *testing.T) {
	ctx := context.Background()
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspaceWithAgent(t, remote)
	home := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, home)
	agent := &resources.Agent{
		Kind: resources.ModuleAgent, Publisher: "codefly.dev", Name: "gitops-test", Version: "1.0.0",
	}
	binary, err := agent.Path(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	generator := `#!/bin/sh
set -eu
module_dir="$1"
destination="$module_dir/deployment/kustomize"
mkdir -p "$destination/overlays/production"
cat > "$destination/overlays/production/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
EOF
cat > "$destination/bundle.json" <<'EOF'
{
  "schemaVersion": "codefly.dev/module-bundle/v1",
  "module": "payments",
  "environments": [{
    "name": "production",
    "namespace": "payments",
    "cluster": "k3d",
    "resourcePath": "overlays/production",
    "services": ["api", "auth-sidecar"]
  }]
}
EOF
`
	if err := os.WriteFile(binary, []byte(generator), 0o755); err != nil {
		t.Fatal(err)
	}
	module, err := workspace.LoadModuleFromName(ctx, "payments")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := orchestration.SelectEnvironment(workspace, "production")
	if err != nil {
		t.Fatal(err)
	}
	err = renderModuleBundle(
		ctx,
		workspace,
		module,
		environment,
		t.TempDir(),
		promotableServiceGraph("payments", []string{"api"}),
	)
	if err == nil || !strings.Contains(err.Error(), "services [api auth-sidecar] differ from rendered in-cluster graph [api]") {
		t.Fatalf("phantom module service error = %v", err)
	}

	environment.Namespace = ""
	err = renderModuleBundle(
		ctx,
		workspace,
		module,
		environment,
		t.TempDir(),
		promotableServiceGraph("payments", []string{"api"}),
	)
	if err == nil || !strings.Contains(err.Error(), "requires an explicit namespace") {
		t.Fatalf("implicit namespace error = %v", err)
	}

	environment.Namespace = "payments"
	environment.Cluster = nil
	err = renderModuleBundle(
		ctx,
		workspace,
		module,
		environment,
		t.TempDir(),
		promotableServiceGraph("payments", []string{"api"}),
	)
	if err == nil || !strings.Contains(err.Error(), "requires an explicit cluster kind") {
		t.Fatalf("implicit cluster error = %v", err)
	}
}

func TestCopyModuleInputTreeExcludesTransientBuildOutput(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "module")
	topology := filepath.Join(source, "deployment", "topology.bindings.codefly.yaml")
	if err := os.MkdirAll(filepath.Dir(topology), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(topology, []byte("version: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nextModules := filepath.Join(source, "services", "frontend", "code", ".next", "node_modules")
	if err := os.MkdirAll(nextModules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(nextModules, "transient-package")); err != nil {
		t.Fatal(err)
	}

	if err := copyModuleInputTree(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "deployment", "topology.bindings.codefly.yaml"))
	if err != nil || string(data) != "version: v1\n" {
		t.Fatalf("module contract was not staged: data=%q error=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "services", "frontend", "code", ".next")); !os.IsNotExist(err) {
		t.Fatalf("transient Next.js output was staged: %v", err)
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
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workspace.Dir(), ".codefly", "gitops", "publications", "payments-production.json")); err != nil {
		t.Fatal(err)
	}
	retryPlan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if len(retryPlan.Changed) != 0 || retryPlan.ExistingCommit != first.Commit {
		t.Fatalf("retry plan = %+v", retryPlan)
	}
	retried, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: retryPlan.ID}, preparedPermit)
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

func TestPublishPreservesSnapshotLineageAfterSquashAndBranchDeletion(t *testing.T) {
	ctx := context.Background()
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	configureSSHSigning(t)
	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true,
		PromotionBranch: "codefly/promote-payments-production",
	}
	firstPlan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: firstPlan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	gitRun(t, "", "clone", remote, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	gitRun(t, work, "merge", "--squash", "origin/"+request.PromotionBranch)
	gitRun(t, work, "commit", "-m", "squash promotion")
	gitRun(t, work, "push", "origin", "main")
	gitRun(t, work, "push", "origin", "--delete", request.PromotionBranch)

	renderPublishFixture(t, workspace.Dir(), "payments", "production", "worker")
	secondPlan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: secondPlan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, "", "--git-dir", remote, "merge-base", "--is-ancestor", first.SnapshotRevision, second.SnapshotRevision)
	if first.SnapshotRevision == second.SnapshotRevision {
		t.Fatal("changed service tree reused the previous snapshot")
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

	_, err := PlanPublish(context.Background(), workspace, &PublishRequest{
		Module: "payments", Environment: "production", Local: true,
		PromotionBranch: "codefly/promote-payments-production",
	})
	if err == nil || !strings.Contains(err.Error(), "unrelated change") {
		t.Fatalf("plan error = %v", err)
	}
}

func TestPublishRejectsRenderOutsideTheExactModuleServiceGraph(t *testing.T) {
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspaceWithServices(t, remote, []string{"api", "worker"})
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")

	_, err := PlanPublish(context.Background(), workspace, &PublishRequest{
		Module: "payments", Environment: "production", Local: true,
	})
	if err == nil || !strings.Contains(err.Error(), "differs from module service graph") {
		t.Fatalf("service graph error = %v", err)
	}
}

func TestPlanPublishRejectsRepositorySymlinkWithoutTouchingItsTarget(t *testing.T) {
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")

	victim := t.TempDir()
	victimFile := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(victimFile, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	gitRun(t, "", "clone", remote, work)
	gitRun(t, work, "config", "user.name", "Codefly Test")
	gitRun(t, work, "config", "user.email", "codefly@example.com")
	gitRun(t, work, "config", "commit.gpgsign", "false")
	targetParent := filepath.Join(work, "environments", "deployments", "modules")
	if err := os.MkdirAll(targetParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(targetParent, "payments")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "environments")
	gitRun(t, work, "commit", "-m", "seed hostile publication path")
	gitRun(t, work, "push", "origin", "main")

	_, err := PlanPublish(context.Background(), workspace, &PublishRequest{
		Module: "payments", Environment: "production", Local: true,
	})
	if err == nil || !strings.Contains(err.Error(), "traverses symbolic link") {
		t.Fatalf("symlink publication error = %v", err)
	}
	data, readErr := os.ReadFile(victimFile)
	if readErr != nil || string(data) != "keep\n" {
		t.Fatalf("external target changed: data=%q err=%v", data, readErr)
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
	firstPlan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: firstPlan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	mergePromotionToMain(t, remote, request.PromotionBranch)

	renderPublishFixture(t, workspace.Dir(), "payments", "production", "worker")
	secondPlan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: secondPlan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if second.RenderDigest == first.RenderDigest {
		t.Fatal("second promotion did not change the rendered tree")
	}
	mergePromotionToMain(t, remote, request.PromotionBranch)
	if err := writeReceipt(workspace.Dir(), "evidence", "first.json", Evidence{
		SchemaVersion: EvidenceSchemaVersion, Module: "payments", Environment: "production",
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
	rollbackPlan, err := PlanRollback(ctx, workspace, &rollbackRequest)
	if err != nil {
		t.Fatal(err)
	}
	if rollbackPlan.RenderDigest != first.RenderDigest {
		t.Fatalf("rollback digest = %s, want %s", rollbackPlan.RenderDigest, first.RenderDigest)
	}
	rollback, err := Rollback(ctx, workspace, &RollbackMutation{Request: rollbackRequest, PlanID: rollbackPlan.ID}, preparedPermit)
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
		SchemaVersion: EvidenceSchemaVersion, Module: "other", Environment: "production",
		SignedCommit: revision, ArgoRevision: revision, Health: "Healthy",
		Review: ReviewEvidence{
			State: "LOCAL_REVIEW_REF", ReviewDecision: "LOCAL_QUALIFIED",
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := PlanRollback(context.Background(), workspace, &RollbackRequest{
		PublishRequest: PublishRequest{
			Module: "payments", Environment: "production", Local: true,
		},
		ToRevision: revision,
	})
	if err == nil || !strings.Contains(err.Error(), "no reviewed Healthy promotion evidence") {
		t.Fatalf("rollback evidence error = %v", err)
	}
}

func TestRollbackRequiresTheReviewedPublicationCommitNotItsServiceSnapshot(t *testing.T) {
	root := t.TempDir()
	snapshot := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := writeReceipt(root, "evidence", "snapshot.json", Evidence{
		SchemaVersion: SchemaVersion, Module: "payments", Environment: "production",
		SignedCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ArgoRevision: snapshot, Health: "Healthy",
		Review: ReviewEvidence{
			State: "LOCAL_REVIEW_REF", ReviewDecision: "LOCAL_QUALIFIED",
		},
	}); err != nil {
		t.Fatal(err)
	}
	err := requireReviewedRevision(root, "payments", "production", snapshot)
	if err == nil || !strings.Contains(err.Error(), "no reviewed Healthy promotion evidence") {
		t.Fatalf("snapshot rollback error = %v", err)
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

func TestPlanPublishRejectsLocalQualificationForRemoteEnvironment(t *testing.T) {
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	workspace.Environments = append(workspace.Environments, &resources.Environment{
		Name:    "aws",
		Cluster: &resources.EnvironmentCluster{Kind: "eks"},
	})
	renderPublishFixture(t, workspace.Dir(), "payments", "aws", "api")

	_, err := PlanPublish(context.Background(), workspace, &PublishRequest{
		Module: "payments", Environment: "aws", Local: true,
	})
	if err == nil || !strings.Contains(err.Error(), "requires a k3d environment") {
		t.Fatalf("remote environment local qualification error = %v", err)
	}
}

func TestPlanPublishRejectsRemoteRepositoryForLocalQualification(t *testing.T) {
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	workspace.Gitops.RepoURL = "https://github.com/codefly-test/manifests.git"
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")

	_, err := PlanPublish(context.Background(), workspace, &PublishRequest{
		Module: "payments", Environment: "production", Local: true,
	})
	if err == nil || !strings.Contains(err.Error(), "requires an absolute file repository URL") {
		t.Fatalf("remote repository local qualification error = %v", err)
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
	return loadGitopsWorkspaceWithServices(t, remote, []string{"api"})
}

func loadGitopsWorkspaceWithServices(t *testing.T, remote string, services []string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	var serviceReferences strings.Builder
	for _, service := range services {
		fmt.Fprintf(&serviceReferences, "  - name: %s\n", service)
	}
	config := fmt.Sprintf(`name: payments
layout: flat
services:
%s
environments:
  - name: production
    cluster:
      kind: k3d
gitops:
  repo-url: file://%s
  fetch-repo-url: https://host.k3d.internal/manifests.git
  path: environments
  branch: main
`, serviceReferences.String(), remote)
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func loadGitopsWorkspaceWithAgent(t *testing.T, remote string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	config := fmt.Sprintf(`name: workspace
layout: modules
modules:
  - name: payments
environments:
  - name: production
    namespace: payments
    cluster:
      kind: k3d
    gitops:
      repo-url: file://%s
      fetch-repo-url: https://host.k3d.internal/manifests.git
      path: environments
      branch: main
`, remote)
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	module := `kind: module
name: payments
agent:
  kind: codefly:module
  publisher: codefly.dev
  name: gitops-test
  version: 1.0.0
services:
  - name: api
`
	moduleDir := filepath.Join(root, "modules", "payments")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, resources.ModuleConfigurationName), []byte(module), 0o644); err != nil {
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
	destination := filepath.Join(root, "deployments", "modules", module)
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: destination, Module: module, Services: []string{"api"},
		OwnedPath:    filepath.ToSlash(filepath.Join("environments", "deployments", "modules", module)),
		ServiceGraph: promotableServiceGraph(module, []string{"api"}),
		Environment:  environment, Namespace: "payments", AppProject: "payments", Promotable: true,
	}, func(ctx context.Context, stage string) error {
		return writePublishServiceFixture(stage, environment, name)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func renderPublishFixtureWithModuleAgent(t *testing.T, workspace *resources.Workspace, moduleName, environmentName, name string) {
	t.Helper()
	ctx := context.Background()
	module, err := workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := orchestration.SelectEnvironment(workspace, environmentName)
	if err != nil {
		t.Fatal(err)
	}
	graph := promotableServiceGraph(moduleName, []string{"api"})
	destination := filepath.Join(workspace.Dir(), "deployments", "modules", moduleName)
	_, err = RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination, Module: moduleName, Services: []string{"api"},
		OwnedPath:    filepath.ToSlash(filepath.Join("environments", "deployments", "modules", moduleName)),
		ModulePath:   "module",
		ServiceGraph: graph,
		Environment:  environmentName, Namespace: environment.Namespace,
		AppProject: "payments", Promotable: true,
	}, func(ctx context.Context, stage string) error {
		if err := writePublishServiceFixture(stage, environmentName, name); err != nil {
			return err
		}
		return renderModuleBundle(ctx, workspace, module, environment, filepath.Join(stage, "module"), graph)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writePublishServiceFixture(stage, environment, name string) error {
	manifest := strings.Replace(pinnedDeployment, "name: api", "name: "+name, 2)
	service := filepath.Join(stage, "services", "api", "overlays", environment)
	if err := os.MkdirAll(service, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(service, "deployment.yaml"), []byte(manifest), 0o644); err != nil {
		return err
	}
	return os.WriteFile(
		filepath.Join(service, "kustomization.yaml"),
		[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"),
		0o644,
	)
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
