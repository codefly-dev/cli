package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	cellDenial = `Error creating: pods "api-5d9f6c7b8-" is forbidden: ValidatingAdmissionPolicy 'cell-admission' with binding 'cell-admission' denied request: no approved execution matches this pod`
	quotaFail  = `Error creating: pods "api-5d9f6c7b8-" is forbidden: exceeded quota: compute-resources, requested: pods=1, used: pods=4, limited: pods=4`
	syncBegan  = "2026-11-03T10:00:00Z"
)

func TestObserveEndsOnAnAdmissionDenialAndPrintsItVerbatim(t *testing.T) {
	request := observedPublication(t)
	request.Kubeconfig = "/tmp/blue.kubeconfig"
	request.KubeContext = "gke_blue"
	var out bytes.Buffer
	request.Output = &out
	installFakeArgoSequence(t, argoProjectJSON(request.Repository), progressingApplication(request))
	calls := installFakeKubectl(t, eventsJSON("ReplicaSet", "api-5d9f6c7b8", "FailedCreate", cellDenial), replicaSetsJSON("api-5d9f6c7b8", "api"))

	_, err := Observe(context.Background(), &request)
	if err == nil {
		t.Fatal("expected the observation to end on the denial")
	}
	want := "payments/Deployment/api FailedCreate: " + cellDenial
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "no retry creates them") {
		t.Fatalf("error = %v", err)
	}
	if got := strings.Count(out.String(), want); got != 1 {
		t.Fatalf("refusal printed %d time(s) in %q, want once", got, out.String())
	}
	log, readErr := os.ReadFile(calls)
	if readErr != nil {
		t.Fatal(readErr)
	}
	first := strings.SplitN(string(log), "\n", 2)[0]
	if !strings.HasPrefix(first, "--kubeconfig /tmp/blue.kubeconfig --context gke_blue get events -n payments -o json") {
		t.Fatalf("kubectl was called as %q, want the environment's kubeconfig and context", first)
	}
}

func TestObservePrintsAFailedCreateOnceAndKeepsWaitingForHealth(t *testing.T) {
	request := observedPublication(t)
	request.Timeout = 30 * time.Second
	var out bytes.Buffer
	request.Output = &out
	healthy := argoApplicationJSON("payments-api", request.Repository, observedServicePath(request), request.Revision, "Healthy", "Succeeded")
	installFakeArgoSequence(t, argoProjectJSON(request.Repository), progressingApplication(request), progressingApplication(request), progressingApplication(request), healthy)
	calls := installFakeKubectl(t, eventsJSON("ReplicaSet", "api-5d9f6c7b8", "FailedCreate", quotaFail), replicaSetsJSON("api-5d9f6c7b8", "api"))

	if _, err := Observe(context.Background(), &request); err != nil {
		t.Fatalf("a FailedCreate the controller retries past is not a verdict: %v", err)
	}
	want := "payments/Deployment/api FailedCreate: " + quotaFail + "\n"
	if out.String() != want {
		t.Fatalf("output = %q, want exactly one line %q", out.String(), want)
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if sweeps := strings.Count(string(log), "get events"); sweeps < 2 {
		t.Fatalf("events read %d time(s) across three pending sweeps, want every pending sweep", sweeps)
	}
}

func TestObserveTimeoutNamesTheWorkloadsItWaitedOnAndTheirRefusals(t *testing.T) {
	request := observedPublication(t)
	request.Timeout = 2 * time.Second
	request.PollInterval = 20 * time.Millisecond
	installFakeArgoSequence(t, argoProjectJSON(request.Repository), progressingApplication(request))
	installFakeKubectl(t, eventsJSON("ReplicaSet", "api-5d9f6c7b8", "FailedCreate", quotaFail), replicaSetsJSON("api-5d9f6c7b8", "api"))

	_, err := Observe(context.Background(), &request)
	if err == nil {
		t.Fatal("expected the observation to time out")
	}
	for _, want := range []string{
		"timed out",
		"still waiting on payments/Deployment/api health=Progressing: Waiting for rollout to finish: 0 of 1 updated replicas are available",
		"the cluster refused payments/Deployment/api FailedCreate: " + quotaFail,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestObserveAttributesStatefulSetAndJobEventsAndIgnoresOtherWorkloads(t *testing.T) {
	request := observedPublication(t)
	var out bytes.Buffer
	request.Output = &out
	application := fmt.Sprintf(`{
  "metadata":{"name":"payments-api"},
  "spec":{"project":"payments","source":{"repoURL":%q,"path":%q,"targetRevision":%q},"destination":{"server":"https://cluster.example.com","namespace":"payments"}},
  "status":{
    "sync":{"status":"Synced","revision":%q},
    "health":{"status":"Progressing"},
    "operationState":{"phase":"Succeeded","syncResult":{"revision":%q}},
    "resources":[
      {"group":"apps","kind":"StatefulSet","namespace":"payments","name":"store","health":{"status":"Progressing"}},
      {"group":"batch","kind":"Job","namespace":"payments","name":"migrate","health":{"status":"Progressing"}}
    ]
  }
}`, request.Repository, observedServicePath(request), request.Revision, request.Revision, request.Revision)
	installFakeArgoSequence(t, projectAllowingWorkloads(request.Repository), application)
	events := fmt.Sprintf(`{"items":[%s,%s,%s]}`,
		eventJSON("StatefulSet", "store", "FailedCreate", "create Pod store-0 in StatefulSet store failed error: "+cellDenial),
		eventJSON("Job", "migrate", "FailedCreate", "Error creating: "+cellDenial),
		eventJSON("Job", "someone-elses", "FailedCreate", "Error creating: "+cellDenial),
	)
	installFakeKubectl(t, events, `{"items":[]}`)

	_, err := Observe(context.Background(), &request)
	if err == nil {
		t.Fatal("expected the observation to end on the denials")
	}
	if !strings.Contains(err.Error(), "the pods of 2 workload(s)") {
		t.Fatalf("error = %v", err)
	}
	for _, want := range []string{"payments/StatefulSet/store FailedCreate: create Pod store-0", "payments/Job/migrate FailedCreate: Error creating"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q lacks %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), "someone-elses") {
		t.Fatalf("output %q names a workload the publication did not deliver", out.String())
	}
}

func TestObserveIgnoresRefusalsRecordedBeforeTheSyncBegan(t *testing.T) {
	request := observedPublication(t)
	request.Timeout = 30 * time.Second
	var out bytes.Buffer
	request.Output = &out
	healthy := argoApplicationJSON("payments-api", request.Repository, observedServicePath(request), request.Revision, "Healthy", "Succeeded")
	installFakeArgoSequence(t, argoProjectJSON(request.Repository), progressingApplication(request), progressingApplication(request), healthy)
	// The previous publication's ReplicaSet, denied by the cell an hour ago and
	// kept in the Deployment's history; this sync's ReplicaSet is rolling out.
	events := fmt.Sprintf(`{"items":[%s,%s]}`,
		eventAtJSON("ReplicaSet", "api-5d9f6c7b8", "FailedCreate", cellDenial, "2026-11-03T09:12:00Z"),
		eventAtJSON("ReplicaSet", "api-7c4b9d2f1", "ScalingReplicaSet", "Scaled up replica set api-7c4b9d2f1 to 1", "2026-11-03T10:00:30Z"),
	)
	installFakeKubectl(t, events, replicaSetsJSON("api-5d9f6c7b8", "api", "api-7c4b9d2f1", "api"))

	if _, err := Observe(context.Background(), &request); err != nil {
		t.Fatalf("a denial of the previous rollout ended this one: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want nothing of a previous rollout", out.String())
	}
}

func TestObserveKeepsARefusalWhoseTimeItCannotRead(t *testing.T) {
	request := observedPublication(t)
	installFakeArgoSequence(t, argoProjectJSON(request.Repository), progressingApplication(request))
	installFakeKubectl(t, eventsJSON("ReplicaSet", "api-5d9f6c7b8", "FailedCreate", cellDenial), replicaSetsJSON("api-5d9f6c7b8", "api"))
	undated := strings.ReplaceAll(eventsJSON("ReplicaSet", "api-5d9f6c7b8", "FailedCreate", cellDenial), `"lastTimestamp":"2026-11-03T10:01:00Z",`, "")
	t.Setenv("CODEFLY_TEST_KUBECTL_EVENTS", undated)

	_, err := Observe(context.Background(), &request)
	if err == nil || !strings.Contains(err.Error(), cellDenial) {
		t.Fatalf("error = %v, want the undated denial shown rather than hidden", err)
	}
}

func TestObserveReadsNoClusterEventsWhileArgoIsHealthy(t *testing.T) {
	request := observedPublication(t)
	installFakeArgo(t, argoProjectJSON(request.Repository), argoApplicationJSON(
		"payments-api", request.Repository, observedServicePath(request), request.Revision, "Healthy", "Succeeded",
	))
	calls := installFakeKubectl(t, `{"items":[]}`, `{"items":[]}`)
	if _, err := Observe(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("kubectl was consulted for a healthy application (stat err = %v)", err)
	}
}

func TestObserveReportsAnUnreadableClusterOnceAndStillAwaitsArgo(t *testing.T) {
	request := observedPublication(t)
	request.Timeout = 2 * time.Second
	request.PollInterval = 20 * time.Millisecond
	var out bytes.Buffer
	request.Output = &out
	installFakeArgoSequence(t, argoProjectJSON(request.Repository), progressingApplication(request))
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\necho called >> \"$CODEFLY_TEST_KUBECTL_CALLS\"\necho 'error: context \"gke_blue\" does not exist' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLY_TEST_KUBECTL_CALLS", calls)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := Observe(context.Background(), &request)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), `events could not be read (kubectl`) {
		t.Fatalf("error = %v", err)
	}
	if got := strings.Count(out.String(), "cannot read the delivered workloads' events"); got != 1 {
		t.Fatalf("unreadable cluster reported %d time(s) in %q, want once", got, out.String())
	}
	log, readErr := os.ReadFile(calls)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Count(string(log), "called") != 1 {
		t.Fatalf("kubectl retried after failing: %q", log)
	}
}

// progressingApplication is the payments-api application still rolling out
// its one Deployment, as Argo CD reports it: Progressing, and nothing of why.
func progressingApplication(request ObserveRequest) string {
	return fmt.Sprintf(`{
  "metadata":{"name":"payments-api"},
  "spec":{"project":"payments","source":{"repoURL":%q,"path":%q,"targetRevision":%q},"destination":{"server":"https://cluster.example.com","namespace":"payments"}},
  "status":{
    "sync":{"status":"Synced","revision":%q},
    "health":{"status":"Progressing"},
    "operationState":{"phase":"Succeeded","startedAt":%q,"syncResult":{"revision":%q}},
    "resources":[{"group":"apps","kind":"Deployment","namespace":"payments","name":"api","health":{"status":"Progressing","message":"Waiting for rollout to finish: 0 of 1 updated replicas are available..."}}]
  }
}`, request.Repository, observedServicePath(request), request.Revision, request.Revision, syncBegan, request.Revision)
}

func projectAllowingWorkloads(repository string) string {
	return fmt.Sprintf(`{
  "metadata":{"name":"payments"},
  "spec":{
    "sourceRepos":[%q],
    "destinations":[{"server":"https://cluster.example.com","namespace":"payments"}],
    "clusterResourceWhitelist":[],
    "namespaceResourceWhitelist":[{"group":"apps","kind":"Deployment"},{"group":"apps","kind":"StatefulSet"},{"group":"batch","kind":"Job"}]
  }
}`, repository)
}

// eventJSON is an event recorded a minute into the sync.
func eventJSON(kind, name, reason, message string) string {
	return eventAtJSON(kind, name, reason, message, "2026-11-03T10:01:00Z")
}

func eventAtJSON(kind, name, reason, message, at string) string {
	return fmt.Sprintf(`{"type":"Warning","reason":%q,"message":%q,"lastTimestamp":%q,"involvedObject":{"kind":%q,"name":%q,"namespace":"payments"}}`, reason, message, at, kind, name)
}

func eventsJSON(kind, name, reason, message string) string {
	return `{"items":[` + eventJSON(kind, name, reason, message) + `]}`
}

func replicaSetsJSON(owned ...string) string {
	items := make([]string, 0, len(owned)/2)
	for i := 0; i+1 < len(owned); i += 2 {
		items = append(items, fmt.Sprintf(`{"metadata":{"name":%q,"ownerReferences":[{"kind":"Deployment","name":%q}]}}`, owned[i], owned[i+1]))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// installFakeArgoSequence answers each `argocd app get` with the next
// application in the sequence, repeating the last one.
func installFakeArgoSequence(t *testing.T, project string, applications ...string) {
	t.Helper()
	bin := t.TempDir()
	dir := t.TempDir()
	for index, application := range applications {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("app-%d.json", index+1)), []byte(application), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "proj" ]; then
  printf '%%s\n' "$CODEFLY_TEST_ARGO_PROJECT"
  exit 0
fi
if [ "$1" = "cluster" ]; then
  printf '%%s\n' "$CODEFLY_TEST_ARGO_CLUSTER"
  exit 0
fi
count=0
if [ -f "$CODEFLY_TEST_ARGO_COUNT" ]; then count=$(cat "$CODEFLY_TEST_ARGO_COUNT"); fi
count=$((count + 1))
printf '%%s' "$count" > "$CODEFLY_TEST_ARGO_COUNT"
if [ "$count" -gt %d ]; then count=%d; fi
cat "$CODEFLY_TEST_ARGO_SEQUENCE/app-$count.json"
`, len(applications), len(applications))
	if err := os.WriteFile(filepath.Join(bin, "argocd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLY_TEST_ARGO_PROJECT", project)
	t.Setenv("CODEFLY_TEST_ARGO_CLUSTER", `{"server":"https://cluster.example.com","name":"test","config":{"tls":true}}`)
	t.Setenv("CODEFLY_TEST_ARGO_COUNT", filepath.Join(dir, "count"))
	t.Setenv("CODEFLY_TEST_ARGO_SEQUENCE", dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// installFakeKubectl answers `kubectl ... get events` and `get replicasets`
// with the given lists and logs every invocation's arguments, one per line,
// to the file it returns.
func installFakeKubectl(t *testing.T, events, replicaSets string) string {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
echo "$@" >> "$CODEFLY_TEST_KUBECTL_CALLS"
case " $* " in
  *" get events "*) printf '%s\n' "$CODEFLY_TEST_KUBECTL_EVENTS"; exit 0 ;;
  *" get replicasets "*) printf '%s\n' "$CODEFLY_TEST_KUBECTL_REPLICASETS"; exit 0 ;;
esac
echo "unexpected kubectl $*" >&2
exit 2
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLY_TEST_KUBECTL_CALLS", calls)
	t.Setenv("CODEFLY_TEST_KUBECTL_EVENTS", events)
	t.Setenv("CODEFLY_TEST_KUBECTL_REPLICASETS", replicaSets)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}
