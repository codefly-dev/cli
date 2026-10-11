package localcluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
)

// jobsAnswer is `kubectl get jobs -o json` for one Job attempt, or none.
func jobsAnswer(uid, condition string) string {
	if uid == "" {
		return `{"items":[]}`
	}
	conditions := []any{}
	if condition != "" {
		conditions = append(conditions, map[string]string{"type": condition, "status": "True"})
	}
	encoded, _ := json.Marshal(map[string]any{"items": []any{map[string]any{
		"metadata": map[string]string{"uid": uid}, "status": map[string]any{"conditions": conditions},
	}}})
	return string(encoded)
}

// TestARunReadsOnlyItsOwnDeliveryAttempt is the attack the review named: an
// unchanged carrier set reuses the Job's name, and the Job an EARLIER run
// created is still there, Complete. The run's answer is the attempt whose UID
// is not the one recorded before it delivered — here a fresh attempt that
// FAILED — read from that attempt's own pods, never the old success.
func TestARunReadsOnlyItsOwnDeliveryAttempt(t *testing.T) {
	answers := []string{jobsAnswer("old", "Complete"), jobsAnswer("old", "Complete"), jobsAnswer("new", ""), jobsAnswer("new", "Failed")}
	var calls []string
	driver := &ClusterDriver{
		AwaitTimeout: time.Minute, Poll: time.Millisecond,
		priorAttempts: map[string]string{"payments/deliver-presence-payments-local-abc": "old"},
		Kubectl: func(_ context.Context, _ string, args ...string) (string, error) {
			call := strings.Join(args, " ")
			calls = append(calls, call)
			if strings.Contains(call, "get jobs") {
				answer := answers[0]
				if len(answers) > 1 {
					answers = answers[1:]
				}
				return answer, nil
			}
			return "refused payments.local.payments.json with 401: token review refused\n", nil
		},
	}
	delivery, err := driver.Await(context.Background(), gitops.DeliveryJob{Name: "deliver-presence-payments-local-abc", Namespace: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Succeeded || delivery.Attempt != "new" {
		t.Fatalf("the run read %+v, want this run's failed attempt", delivery)
	}
	if last := calls[len(calls)-1]; !strings.Contains(last, "logs -l batch.kubernetes.io/controller-uid=new") {
		t.Fatalf("the receipts were not read from this run's attempt: %q", last)
	}
	if len(delivery.Receipts) != 1 || !strings.HasPrefix(delivery.Receipts[0], "refused") {
		t.Fatalf("receipts %v", delivery.Receipts)
	}

	// No attempt of its own ever appears: a timeout, never the old success.
	stuck := &ClusterDriver{
		AwaitTimeout: 5 * time.Millisecond, Poll: time.Millisecond,
		priorAttempts: map[string]string{"payments/job": "old"},
		Kubectl: func(context.Context, string, ...string) (string, error) {
			return jobsAnswer("old", "Complete"), nil
		},
	}
	if delivery, err = stuck.Await(context.Background(), gitops.DeliveryJob{Name: "job", Namespace: "payments"}); err == nil || delivery.Succeeded {
		t.Fatalf("a previous run's Complete Job answered for this run: %+v %v", delivery, err)
	}
}

// TestARunAsksForAFreshAttemptOfAJobAnEarlierRunRan: the attempt that existed
// before the run is recorded, and the Application syncing that Job's overlay
// is asked for a fresh sync — the Sync hook runs again, a new attempt. A Job
// that never ran is left to the sync the bootstrap starts.
func TestARunAsksForAFreshAttemptOfAJobAnEarlierRunRan(t *testing.T) {
	var patched []string
	driver := &ClusterDriver{
		AwaitTimeout: time.Minute, Poll: time.Millisecond,
		Kubectl: func(_ context.Context, _ string, args ...string) (string, error) {
			call := strings.Join(args, " ")
			switch {
			case strings.Contains(call, "metadata.name=deliver-presence-ran"):
				return jobsAnswer("old", "Complete"), nil
			case strings.Contains(call, "get jobs"):
				return jobsAnswer("", ""), nil
			case strings.Contains(call, "get applications"):
				return `{"items":[{"metadata":{"name":"in-cluster-payments-solution-host-bindings"},"spec":{"source":{"path":"env/modules/payments/solution-host-bindings/overlays/local"}}},` +
					`{"metadata":{"name":"in-cluster-payments-api"},"spec":{"source":{"path":"env/modules/payments/services/api/overlays/local"}}}]}`, nil
			case strings.Contains(call, "patch application"):
				patched = append(patched, call)
			}
			return "", nil
		},
	}
	jobs := []gitops.DeliveryJob{
		{Kind: "presence", Name: "deliver-presence-ran", Namespace: "payments", Path: "env/modules/payments/solution-host-bindings/overlays/local/deliver-presence.yaml"},
		{Kind: "authority", Name: "deliver-authority-new", Namespace: "platform-authority", Path: "env/modules/payments/solution-authority/overlays/local/deliver-authority.yaml"},
	}
	if err := driver.recordPriorAttempts(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if err := driver.requestAttempts(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if driver.priorAttempts["payments/deliver-presence-ran"] != "old" || driver.priorAttempts["platform-authority/deliver-authority-new"] != "" {
		t.Fatalf("prior attempts %v", driver.priorAttempts)
	}
	if len(patched) != 1 || !strings.Contains(patched[0], "application in-cluster-payments-solution-host-bindings") || !strings.Contains(patched[0], `"sync":{}`) {
		t.Fatalf("fresh syncs requested: %v", patched)
	}
}
