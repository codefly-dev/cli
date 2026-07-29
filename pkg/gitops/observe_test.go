package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	observedRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	wrongRevision    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	signedCommit     = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestObserveStoresExactHealthyArgoEvidence(t *testing.T) {
	request := observedPublication(t)
	installFakeArgo(t, argoProjectJSON(request.Repository), argoApplicationJSON(
		"payments-api", request.Repository, request.Path, request.Revision, "Healthy", "Succeeded",
	))
	result, err := Observe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Evidence.ArgoRevision != request.Revision || result.Evidence.Health != "Healthy" ||
		result.Evidence.Cluster != "https://cluster.example.com" || result.Evidence.ClusterIdentity == "" {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	if result.Evidence.Repository != request.Repository || result.Evidence.Path != request.Path {
		t.Fatalf("Git evidence = %+v", result.Evidence)
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatalf("evidence file: %v", err)
	}
}

func TestObserveRejectsRevisionMismatchAndSharedResources(t *testing.T) {
	tests := []struct {
		name        string
		application func(ObserveRequest) string
		want        string
	}{
		{
			name: "revision",
			application: func(request ObserveRequest) string {
				return argoApplicationJSON("payments-api", request.Repository, request.Path, wrongRevision, "Healthy", "Succeeded")
			},
			want: "reconciled revision " + wrongRevision,
		},
		{
			name: "shared",
			application: func(request ObserveRequest) string {
				return fmt.Sprintf(`{
  "metadata":{"name":"payments-api"},
  "spec":{"project":"payments","source":{"repoURL":%q,"path":%q},"destination":{"server":"https://cluster.example.com","namespace":"payments"}},
  "status":{"conditions":[{"type":"SharedResourceWarning","message":"Deployment/api is shared"}]}
}`, request.Repository, request.Path)
			},
			want: "shared resources",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := observedPublication(t)
			installFakeArgo(t, argoProjectJSON(request.Repository), test.application(request))
			_, err := Observe(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestObserveRejectsSourceAndProjectAuthorityViolations(t *testing.T) {
	tests := []struct {
		name    string
		project func(ObserveRequest) string
		app     func(ObserveRequest) string
		want    string
	}{
		{
			name:    "source path",
			project: func(request ObserveRequest) string { return argoProjectJSON(request.Repository) },
			app: func(request ObserveRequest) string {
				return argoApplicationJSON("payments-api", request.Repository, "other/path", request.Revision, "Healthy", "Succeeded")
			},
			want: "observes path",
		},
		{
			name: "wildcard source authority",
			project: func(request ObserveRequest) string {
				return strings.Replace(argoProjectJSON(request.Repository), request.Repository, "*", 1)
			},
			app: func(request ObserveRequest) string {
				return argoApplicationJSON("payments-api", request.Repository, request.Path, request.Revision, "Healthy", "Succeeded")
			},
			want: "wildcard source repository authority",
		},
		{
			name:    "cluster resource outside whitelist",
			project: func(request ObserveRequest) string { return argoProjectJSON(request.Repository) },
			app: func(request ObserveRequest) string {
				app := argoApplicationJSON("payments-api", request.Repository, request.Path, request.Revision, "Healthy", "Succeeded")
				return strings.Replace(app, `"resources":[]`, `"resources":[{"group":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"admin"}]`, 1)
			},
			want: "outside AppProject",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := observedPublication(t)
			installFakeArgo(t, test.project(request), test.app(request))
			_, err := Observe(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestObserveRejectsDuplicateApplicationsWithoutPolling(t *testing.T) {
	request := observedPublication(t)
	request.Applications = []string{"payments-api", "payments-api"}
	_, err := Observe(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "selected more than once") {
		t.Fatalf("duplicate application error = %v", err)
	}
}

func TestObserveRejectsPublishedSubtreeDigestMismatchBeforePollingArgo(t *testing.T) {
	request := observedPublication(t)
	request.RenderDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	_, err := Observe(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "reconciled Git tree digest") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}

func TestObserveRechecksHealthyApplicationsUntilOneStableSweep(t *testing.T) {
	request := observedPublication(t)
	bin := t.TempDir()
	counter := filepath.Join(t.TempDir(), "count")
	script := filepath.Join(bin, "argocd")
	content := `#!/bin/sh
if [ "$1" = "proj" ]; then
  printf '%s\n' "$CODEFLY_TEST_ARGO_PROJECT"
  exit 0
fi
if [ "$1" = "cluster" ]; then
  printf '%s\n' "$CODEFLY_TEST_ARGO_CLUSTER"
  exit 0
fi
count=0
if [ -f "$CODEFLY_TEST_ARGO_COUNT" ]; then count=$(cat "$CODEFLY_TEST_ARGO_COUNT"); fi
count=$((count + 1))
printf '%s' "$count" > "$CODEFLY_TEST_ARGO_COUNT"
if [ "$count" = "2" ]; then
  printf '%s\n' "$CODEFLY_TEST_ARGO_DEGRADED"
else
  printf '%s\n' "$CODEFLY_TEST_ARGO_APPLICATION"
fi
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLY_TEST_ARGO_PROJECT", argoProjectJSON(request.Repository))
	t.Setenv("CODEFLY_TEST_ARGO_APPLICATION", argoApplicationJSON(
		"payments-api", request.Repository, request.Path, request.Revision, "Healthy", "Succeeded",
	))
	t.Setenv("CODEFLY_TEST_ARGO_DEGRADED", argoApplicationJSON(
		"payments-api", request.Repository, request.Path, request.Revision, "Degraded", "Succeeded",
	))
	t.Setenv("CODEFLY_TEST_ARGO_CLUSTER", `{"server":"https://cluster.example.com","name":"test","config":{"tls":true}}`)
	t.Setenv("CODEFLY_TEST_ARGO_COUNT", counter)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := Observe(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "4" {
		t.Fatalf("Argo application polls = %s, want 4", data)
	}
}

func TestObserveRejectsUnverifiedLocalReviewReference(t *testing.T) {
	request := observedPublication(t)
	request.PullRequest = request.Repository + "#refs/codefly/reviews/missing"
	installFakeArgo(t, argoProjectJSON(request.Repository), argoApplicationJSON(
		"payments-api", request.Repository, request.Path, request.Revision, "Healthy", "Succeeded",
	))
	if _, err := Observe(context.Background(), request); err == nil || !strings.Contains(err.Error(), "verify local promotion review ref") {
		t.Fatalf("unverified local review error = %v", err)
	}
	request.Local = false
	if _, err := Observe(context.Background(), request); err == nil || !strings.Contains(err.Error(), "allowed only for local qualification") {
		t.Fatalf("remote local-review error = %v", err)
	}
}

func TestObserveReviewProvesApprovalMergeAndPublishedCommit(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "gh")
	content := `#!/bin/sh
printf '%s\n' "$CODEFLY_TEST_GH_RESPONSE"
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEFLY_TEST_GH_RESPONSE", `{
  "url":"https://github.com/codefly-dev/manifests/pull/42",
  "state":"MERGED",
  "reviewDecision":"APPROVED",
  "reviews":[{"state":"APPROVED","author":{"login":"reviewer"}}],
  "mergeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  "commits":[{"oid":"cccccccccccccccccccccccccccccccccccccccc"}]
}`)
	review, err := observeReview(context.Background(),
		"https://github.com/codefly-dev/manifests/pull/42", observedRevision, signedCommit,
		"https://github.com/codefly-dev/manifests.git", false)
	if err != nil {
		t.Fatal(err)
	}
	if review.MergeCommit != observedRevision || len(review.Reviewers) != 1 || review.Reviewers[0] != "reviewer" {
		t.Fatalf("review evidence = %+v", review)
	}
	if _, err := observeReview(context.Background(),
		"https://github.com/codefly-dev/manifests/pull/42", observedRevision, wrongRevision,
		"https://github.com/codefly-dev/manifests.git", false); err == nil {
		t.Fatal("review accepted a commit not present in the pull request")
	}
}

func observedPublication(t *testing.T) ObserveRequest {
	t.Helper()
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	destination := filepath.Join(workspace.Dir(), "deployments", "environments", "local", "modules", "payments")
	_, err := RenderOwnedTree(context.Background(), RenderOptions{
		Destination: destination, Module: "payments", Environment: "local",
		AppProject: "payments", Promotable: true,
	}, func(ctx context.Context, stage string) error {
		manifests := pinnedDeployment + `---
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: payments
  namespace: argocd
spec:
  sourceRepos:
    - https://github.com/codefly-dev/manifests.git
  destinations:
    - namespace: payments
      server: https://cluster.example.com
`
		return os.WriteFile(filepath.Join(stage, "manifests.yaml"), []byte(manifests), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	configureSSHSigning(t)
	publish := PublishRequest{
		Module: "payments", Environment: "local", Local: true,
		PromotionBranch: "codefly/promote-payments-local",
	}
	plan, err := PlanPublish(context.Background(), workspace, publish)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Publish(context.Background(), workspace, PublishMutation{Request: publish, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	return ObserveRequest{
		WorkspaceRoot: workspace.Dir(), Module: "payments", Environment: "local",
		AppProject: "payments", Applications: []string{"payments-api"},
		Repository: result.Repository, Path: result.Path,
		Revision: result.Commit, Commit: result.Commit, Tree: result.Tree,
		RenderDigest: result.RenderDigest, PullRequest: result.PullRequest, Local: true,
		Timeout: time.Second, PollInterval: time.Millisecond,
	}
}

func argoProjectJSON(repository string) string {
	return fmt.Sprintf(`{
  "metadata":{"name":"payments"},
  "spec":{
    "sourceRepos":[%q],
    "destinations":[{"server":"https://cluster.example.com","namespace":"payments"}],
    "clusterResourceWhitelist":[],
    "namespaceResourceWhitelist":[{"group":"apps","kind":"Deployment"}]
  }
}`, repository)
}

func argoApplicationJSON(name, repository, path, revision, health, operation string) string {
	return fmt.Sprintf(`{
  "metadata":{"name":%q},
  "spec":{
    "project":"payments",
    "source":{"repoURL":%q,"path":%q,"targetRevision":"main"},
    "destination":{"server":"https://cluster.example.com","namespace":"payments"}
  },
  "status":{
    "sync":{"status":"Synced","revision":%q},
    "health":{"status":%q},
    "operationState":{"phase":%q,"syncResult":{"revision":%q}},
    "resources":[]
  }
}`, name, repository, path, revision, health, operation, revision)
}

func installFakeArgo(t *testing.T, project, application string) {
	t.Helper()
	bin := t.TempDir()
	script := filepath.Join(bin, "argocd")
	content := `#!/bin/sh
if [ "$1" = "proj" ]; then
  printf '%s\n' "$CODEFLY_TEST_ARGO_PROJECT"
  exit 0
fi
if [ "$1" = "app" ]; then
  printf '%s\n' "$CODEFLY_TEST_ARGO_APPLICATION"
  exit 0
fi
if [ "$1" = "cluster" ]; then
  printf '%s\n' "$CODEFLY_TEST_ARGO_CLUSTER"
  exit 0
fi
exit 2
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLY_TEST_ARGO_PROJECT", project)
	t.Setenv("CODEFLY_TEST_ARGO_APPLICATION", application)
	t.Setenv("CODEFLY_TEST_ARGO_CLUSTER", `{"server":"https://cluster.example.com","name":"test","config":{"tls":true}}`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
