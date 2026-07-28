package gitops

import (
	"context"
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
	publishedTree    = "dddddddddddddddddddddddddddddddddddddddd"
	renderDigest     = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

func TestObserveStoresExactHealthyArgoEvidence(t *testing.T) {
	root := t.TempDir()
	installFakeArgo(t, `{
  "metadata":{"name":"payments"},
  "spec":{"destinations":[{"server":"https://cluster.example.com","namespace":"payments"}]}
}`, `{
  "metadata":{"name":"payments-api"},
  "spec":{"project":"payments","destination":{"server":"https://cluster.example.com","namespace":"payments"}},
  "status":{
    "sync":{"status":"Synced","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "health":{"status":"Healthy"},
    "operationState":{"phase":"Succeeded","syncResult":{"revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
  }
}`)
	result, err := Observe(context.Background(), ObserveRequest{
		WorkspaceRoot: root, Module: "payments", Environment: "local",
		AppProject: "payments", Applications: []string{"payments-api"},
		Revision: observedRevision, Commit: signedCommit, Tree: publishedTree,
		RenderDigest: renderDigest, PullRequest: "file:///tmp/repo.git#refs/codefly/reviews/payments",
		Timeout: time.Second, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Evidence.ArgoRevision != observedRevision || result.Evidence.Health != "Healthy" || result.Evidence.Cluster != "https://cluster.example.com" {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatalf("evidence file: %v", err)
	}
}

func TestObserveRejectsRevisionMismatchAndSharedResources(t *testing.T) {
	project := `{
  "metadata":{"name":"payments"},
  "spec":{"destinations":[{"server":"https://cluster.example.com","namespace":"payments"}]}
}`
	tests := []struct {
		name string
		app  string
		want string
	}{
		{
			name: "revision",
			app: `{
  "metadata":{"name":"payments-api"},
  "spec":{"project":"payments","destination":{"server":"https://cluster.example.com","namespace":"payments"}},
  "status":{"sync":{"status":"Synced","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"health":{"status":"Healthy"},"operationState":{"phase":"Succeeded"}}
}`,
			want: "reconciled revision " + wrongRevision,
		},
		{
			name: "shared",
			app: `{
  "metadata":{"name":"payments-api"},
  "spec":{"project":"payments","destination":{"server":"https://cluster.example.com","namespace":"payments"}},
  "status":{"conditions":[{"type":"SharedResourceWarning","message":"Deployment/api is shared"}]}
}`,
			want: "shared resources",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installFakeArgo(t, project, test.app)
			_, err := Observe(context.Background(), ObserveRequest{
				WorkspaceRoot: t.TempDir(), Module: "payments", Environment: "local",
				AppProject: "payments", Applications: []string{"payments-api"},
				Revision: observedRevision, Commit: signedCommit, Tree: publishedTree,
				RenderDigest: renderDigest, PullRequest: "file:///tmp/repo.git#review",
				Timeout: time.Second, PollInterval: time.Millisecond,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestObserveRejectsApplicationOutsideProjectDestination(t *testing.T) {
	installFakeArgo(t, `{
  "metadata":{"name":"payments"},
  "spec":{"destinations":[{"server":"https://cluster.example.com","namespace":"payments"}]}
}`, `{
  "metadata":{"name":"payments-api"},
  "spec":{"project":"payments","destination":{"server":"https://other.example.com","namespace":"payments"}},
  "status":{}
}`)
	_, err := Observe(context.Background(), ObserveRequest{
		WorkspaceRoot: t.TempDir(), Module: "payments", Environment: "local",
		AppProject: "payments", Applications: []string{"payments-api"},
		Revision: observedRevision, Commit: signedCommit, Tree: publishedTree,
		RenderDigest: renderDigest, PullRequest: "file:///tmp/repo.git#review",
		Timeout: time.Second, PollInterval: time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "outside AppProject") {
		t.Fatalf("error = %v", err)
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
		"https://github.com/codefly-dev/manifests/pull/42", observedRevision, signedCommit)
	if err != nil {
		t.Fatal(err)
	}
	if review.MergeCommit != observedRevision || len(review.Reviewers) != 1 || review.Reviewers[0] != "reviewer" {
		t.Fatalf("review evidence = %+v", review)
	}
	if _, err := observeReview(context.Background(),
		"https://github.com/codefly-dev/manifests/pull/42", observedRevision, wrongRevision); err == nil {
		t.Fatal("review accepted a commit not present in the pull request")
	}
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
exit 2
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLY_TEST_ARGO_PROJECT", project)
	t.Setenv("CODEFLY_TEST_ARGO_APPLICATION", application)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
