package localcluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
)

// Regression assertions invert the review probes: each attack must be refused.
func TestReviewECFAcceptsForeignApplicationOwner(t *testing.T) {
	for _, foreignOwner := range []bool{false, true} {
		argo := newFakeArgo("https://kubernetes.default.svc")
		delete(argo.app["metadata"].(map[string]any), "ownerReferences")
		if foreignOwner {
			argo.app["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet", "name": "another-tenant", "uid": "foreign-set-uid", "controller": true}}
		}
		driver := &ClusterDriver{AwaitTimeout: 10 * time.Millisecond, Poll: time.Millisecond, nonce: "review", Kubectl: argo.run}
		if err := driver.acquireAttempts(context.Background(), []gitops.DeliveryJob{{Name: "delivery", Namespace: "payments", Application: deliveryApplication()}}); err == nil {
			t.Fatal("foreign/unowned Application acquired a sync")
		}
		if len(argo.patches) != 0 {
			t.Fatalf("sync calls: %v", argo.patches)
		}
	}
}

func TestReviewECFAcceptsMultiSourceOverride(t *testing.T) {
	argo := newFakeArgo("https://kubernetes.default.svc")
	argo.app["spec"].(map[string]any)["sources"] = []any{map[string]any{
		"repoURL": "https://unrelated.example.test/repo.git", "targetRevision": "other", "path": "unrelated",
	}}
	driver := &ClusterDriver{AwaitTimeout: 10 * time.Millisecond, Poll: time.Millisecond, nonce: "review", Kubectl: argo.run}
	if err := driver.acquireAttempts(context.Background(), []gitops.DeliveryJob{{Name: "delivery", Namespace: "payments", Application: deliveryApplication()}}); err == nil {
		t.Fatal("a multi-source override acquired a sync")
	}
	if len(argo.patches) != 0 {
		t.Fatalf("sync calls: %v", argo.patches)
	}
}

func TestReviewECFSameSecondForeignJobAnswersAcceptedSync(t *testing.T) {
	argo := newFakeArgo("https://kubernetes.default.svc")
	job := gitops.DeliveryJob{Name: "delivery", Namespace: "payments", Application: deliveryApplication()}
	reads := 0
	driver := &ClusterDriver{AwaitTimeout: 10 * time.Millisecond, Poll: time.Millisecond, nonce: "review"}
	driver.Kubectl = func(ctx context.Context, stdin string, args ...string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "get jobs"):
			reads++
			if reads == 1 {
				return jobsAnswer("old", "2026-10-10T11:59:59Z", "Complete"), nil
			}
			// A previous sync completes a NEW Job after recordPriorAttempts,
			// just before our sync starts in the same RFC3339 second.
			return jobsAnswer("foreign-before-own-sync", syncStarted, "Complete"), nil
		case strings.Contains(call, "logs"):
			return "delivered foreign.json: {}\n", nil
		}
		return argo.run(ctx, stdin, args...)
	}
	if err := driver.recordPriorAttempts(context.Background(), []gitops.DeliveryJob{job}); err != nil {
		t.Fatal(err)
	}
	if err := driver.acquireAttempts(context.Background(), []gitops.DeliveryJob{job}); err != nil {
		t.Fatal(err)
	}
	// Argo has only accepted the new operation; it has not recreated a hook.
	state := argo.app["status"].(map[string]any)["operationState"].(map[string]any)
	if state["phase"] != "Running" {
		t.Fatal("own sync should only just have started")
	}
	delivery, err := driver.Await(context.Background(), &job)
	if err == nil || delivery.Succeeded || delivery.Attempt != "" {
		t.Fatalf("foreign Job answered the accepted sync: %+v, %v", delivery, err)
	}
}
