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
func jobsAnswer(uid, created, condition string, nonces ...string) string {
	if uid == "" {
		return `{"items":[]}`
	}
	conditions := []any{}
	if condition != "" {
		conditions = append(conditions, map[string]string{"type": condition, "status": "True"})
	}
	nonce := ""
	if len(nonces) > 0 {
		nonce = nonces[0]
	}
	encoded, _ := json.Marshal(map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"uid": uid, "creationTimestamp": created, "annotations": map[string]string{deliveryAttemptAnnotation: nonce}},
		"status":   map[string]any{"conditions": conditions},
	}}})
	return string(encoded)
}

const syncStarted = "2026-10-10T12:00:00Z"

// TestARunReadsOnlyItsOwnDeliveryAttempt: an unchanged carrier set reuses
// the Job's name, and the Job an EARLIER run created is still there,
// Complete. The run's answer is the attempt with another UID created no
// earlier than its own accepted sync — here a fresh attempt that FAILED,
// read from that attempt's own pods — never the old success, and never an
// attempt older than the sync.
func TestARunReadsOnlyItsOwnDeliveryAttempt(t *testing.T) {
	started, _ := time.Parse(time.RFC3339, syncStarted)
	key := "payments/deliver-presence-payments-local-abc"
	answers := []string{
		jobsAnswer("old", "2026-10-10T11:00:00Z", "Complete"),
		jobsAnswer("stray", "2026-10-10T11:59:00Z", "Complete"), // another UID, but older than this run's sync
		jobsAnswer("new", "2026-10-10T12:00:05Z", "", "accepted"),
		jobsAnswer("new", "2026-10-10T12:00:05Z", "Failed", "accepted"),
	}
	var calls []string
	driver := &ClusterDriver{
		AwaitTimeout: time.Minute, Poll: time.Millisecond,
		attemptNonces: map[string]string{key: "accepted"},
		priorAttempts: map[string]string{key: "old"}, attemptStarts: map[string]time.Time{key: started},
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
	delivery, err := driver.Await(context.Background(), &gitops.DeliveryJob{Name: "deliver-presence-payments-local-abc", Namespace: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Succeeded || delivery.Attempt != "new" {
		t.Fatalf("the run read %+v, want this run's failed attempt", delivery)
	}
	if last := calls[len(calls)-1]; !strings.Contains(last, "logs -l batch.kubernetes.io/controller-uid=new") {
		t.Fatalf("the receipts were not read from this run's attempt: %q", last)
	}

	stuck := &ClusterDriver{
		AwaitTimeout: 5 * time.Millisecond, Poll: time.Millisecond,
		attemptNonces: map[string]string{"payments/job": "accepted"},
		priorAttempts: map[string]string{"payments/job": "old"}, attemptStarts: map[string]time.Time{"payments/job": started},
		Kubectl: func(context.Context, string, ...string) (string, error) {
			return jobsAnswer("old", "2026-10-10T11:00:00Z", "Complete"), nil
		},
	}
	if delivery, err = stuck.Await(context.Background(), &gitops.DeliveryJob{Name: "job", Namespace: "payments"}); err == nil || delivery.Succeeded {
		t.Fatalf("a previous run's Complete Job answered for this run: %+v %v", delivery, err)
	}
}

// fakeArgo is an Application as Argo CD's controller treats it: an operation
// requested in .operation is taken into status.operationState when the slot
// is free; one requested while another runs is consumed by that one
// (Argo resumes the running operation and clears .operation).
type fakeArgo struct {
	app       map[string]any
	running   int // reads left before the foreign operation finishes
	consume   int // requests to drop without running them
	patches   []string
	reads     int
	conflicts int // patches to refuse as stale
	// busyPatches counts requests made while another operation held the
	// slot — requests Argo CD would let that operation consume.
	busyPatches int
}

func newFakeArgo(server string) *fakeArgo {
	return &fakeArgo{app: map[string]any{
		"metadata": map[string]any{"name": "in-cluster-payments-solution-host-bindings", "resourceVersion": "1", "ownerReferences": []any{map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet", "name": "payments-local", "uid": "applied-set", "controller": true}}},
		"spec": map[string]any{
			"source":      map[string]any{"repoURL": "https://fetch.example.test/repo.git", "targetRevision": "abc", "path": "env/payments/solution-host-bindings/overlays/local"},
			"destination": map[string]any{"server": server, "namespace": "payments"},
		},
		"status": map[string]any{},
	}}
}

func (argo *fakeArgo) run(_ context.Context, _ string, args ...string) (string, error) {
	call := strings.Join(args, " ")
	switch {
	case strings.Contains(call, "get applications"):
		argo.reads++
		status := argo.app["status"].(map[string]any)
		if argo.running > 0 {
			argo.running--
			status["operationState"] = map[string]any{"phase": "Running", "startedAt": "2026-10-10T11:59:00Z",
				"operation": map[string]any{"info": []any{map[string]string{"name": attemptInfo, "value": "someone-else"}}}}
		} else if state, ok := status["operationState"].(map[string]any); ok && state["phase"] == "Running" {
			state["phase"] = "Succeeded"
		}
		if operation, ok := argo.app["operation"].(map[string]any); ok && argo.running == 0 {
			delete(argo.app, "operation")
			if argo.consume > 0 {
				argo.consume--
			} else {
				status["operationState"] = map[string]any{"phase": "Running", "startedAt": syncStarted, "operation": operation}
			}
		}
		encoded, _ := json.Marshal(map[string]any{"items": []any{argo.app}})
		return string(encoded), nil
	case strings.Contains(call, "patch application"):
		argo.patches = append(argo.patches, call)
		if argo.running > 0 {
			argo.busyPatches++
		}
		if argo.conflicts > 0 {
			argo.conflicts--
			return "", context.DeadlineExceeded
		}
		var patch map[string]any
		_ = json.Unmarshal([]byte(args[len(args)-1]), &patch)
		argo.app["operation"] = patch["operation"]
	}
	return "", nil
}

func deliveryApplication() gitops.DeliveryApplication {
	return gitops.DeliveryApplication{
		Name: "in-cluster-payments-solution-host-bindings", RepoURL: "https://fetch.example.test/repo.git",
		TargetRevision: "abc", Path: "env/payments/solution-host-bindings/overlays/local",
		Server: "https://kubernetes.default.svc", Namespace: "payments",
		Owner: gitops.ApplicationSetIdentity{APIVersion: "argoproj.io/v1alpha1", Kind: "ApplicationSet", Name: "payments-local", Namespace: "argocd", UID: "applied-set"},
	}
}

// TestARunAcquiresAFreshSyncDeterministically: the run waits for the
// Application's operation slot to be idle, requests a sync carrying its
// nonce conditionally on the version it read, and holds out until Argo CD's
// operation state carries that nonce — re-requesting one that a stale
// version refused or another operation consumed. A Job an earlier run left
// is no condition: a deleted one (no prior attempt) is synced fresh too.
func TestARunAcquiresAFreshSyncDeterministically(t *testing.T) {
	argo := newFakeArgo("https://kubernetes.default.svc")
	argo.running, argo.consume, argo.conflicts = 2, 1, 1
	driver := &ClusterDriver{AwaitTimeout: time.Minute, Poll: time.Millisecond, nonce: "run", Kubectl: argo.run}
	application := deliveryApplication()
	jobs := []gitops.DeliveryJob{{Name: "deliver-presence-x", Namespace: "payments", Application: application}}
	if err := driver.acquireAttempts(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if got := driver.attemptStarts["payments/deliver-presence-x"].Format(time.RFC3339); got != syncStarted {
		t.Fatalf("the attempt is anchored at %s, want this run's accepted sync %s", got, syncStarted)
	}
	if argo.busyPatches != 0 {
		t.Fatalf("%d sync requests were made while another operation held the slot", argo.busyPatches)
	}
	if len(argo.patches) != 3 {
		t.Fatalf("requested %d syncs, want 3 (one refused as stale, one consumed, one accepted): %v", len(argo.patches), argo.patches)
	}
	for _, patch := range argo.patches {
		if !strings.Contains(patch, `"resourceVersion":"1"`) || !strings.Contains(patch, `"value":"run-in-cluster-payments-solution-host-bindings"`) || !strings.Contains(patch, `"revision":"abc"`) {
			t.Fatalf("the sync request is not conditional, identified and at the revision: %s", patch)
		}
	}
}

// TestARunNeverSyncsAnApplicationThatIsNotTheOneItRendered is the review's
// attack: an Application of the same name whose identity differs — another
// cluster as its destination — is never asked for anything.
func TestARunNeverSyncsAnApplicationThatIsNotTheOneItRendered(t *testing.T) {
	for name, mutate := range map[string]func(*fakeArgo){
		"a remote destination": func(argo *fakeArgo) {
			argo.app["spec"].(map[string]any)["destination"].(map[string]any)["server"] = "https://prod.example.test"
		},
		"another namespace": func(argo *fakeArgo) {
			argo.app["spec"].(map[string]any)["destination"].(map[string]any)["namespace"] = "other"
		},
		"another repository": func(argo *fakeArgo) {
			argo.app["spec"].(map[string]any)["source"].(map[string]any)["repoURL"] = "https://elsewhere.example.test/repo.git"
		},
		"another revision": func(argo *fakeArgo) {
			argo.app["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] = "def"
		},
	} {
		argo := newFakeArgo("https://kubernetes.default.svc")
		mutate(argo)
		driver := &ClusterDriver{AwaitTimeout: 5 * time.Millisecond, Poll: time.Millisecond, nonce: "run", Kubectl: argo.run}
		err := driver.acquireAttempts(context.Background(), []gitops.DeliveryJob{{Name: "j", Namespace: "payments", Application: deliveryApplication()}})
		if err == nil || len(argo.patches) != 0 {
			t.Errorf("%s: the run asked a foreign Application for a sync (%d patches, %v)", name, len(argo.patches), err)
		}
	}
	if err := (&ClusterDriver{}).recordPriorAttempts(context.Background(), []gitops.DeliveryJob{{Name: "j", Namespace: "payments"}}); err == nil {
		t.Error("a Job naming no rendered Application was accepted")
	}
}
