package gitops

// Argo CD reconciles a publication and reports a workload that never comes
// up as Progressing or Degraded, saying nothing of why. The why is on the
// workload's controller: a pod the cell's admission policy refuses is never
// created, so the ReplicaSet, StatefulSet, DaemonSet or Job records a
// FailedCreate event carrying the refusal, and nothing else does — not the
// Deployment, not the Application. Observation reads those events through
// the cluster the environment declares, prints each distinct refusal
// verbatim the first time it is seen, while it is still waiting, and ends
// the wait on an admission denial: no retry creates that pod.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// workloadKinds are the controllers a delivered workload is one of, and so
// the kinds whose events say why its pods are not created.
var workloadKinds = map[string]bool{kindDeployment: true, kindStatefulSet: true, kindDaemonSet: true, kindJob: true}

// deliveredWorkload is one workload an Argo CD application reconciles.
type deliveredWorkload struct {
	Namespace, Kind, Name string
}

func (w deliveredWorkload) String() string { return w.Namespace + "/" + w.Kind + "/" + w.Name }

// workloadRefusal is one event a controller recorded while failing to create
// a pod: the workload it concerns, the reason, and the message exactly as the
// cluster wrote it.
type workloadRefusal struct {
	Workload deliveredWorkload
	Reason   string
	Message  string
}

func (r *workloadRefusal) String() string {
	return fmt.Sprintf("%s %s: %s", r.Workload, r.Reason, r.Message)
}

// Denied reports whether the refusal is an admission denial — a
// ValidatingAdmissionPolicy or a webhook "denied request" — which no retry of
// the controller resolves. It is the denial's own wording that is matched,
// not the "is forbidden" every 403 carries: a quota exceeded or a service
// account the admission cache has not seen yet is forbidden too, and the
// controller's next retry creates that pod.
func (r *workloadRefusal) Denied() bool {
	return strings.Contains(strings.ToLower(r.Message), "denied")
}

// workloadObserver reads the refusals of an observation's delivered workloads
// through kubectl against the cluster the environment declares, and prints
// each distinct one once.
type workloadObserver struct {
	kubeconfig  string
	kubeContext string
	out         io.Writer
	seen        map[string]bool
	// refusals is every distinct refusal seen so far, in the order seen.
	refusals []workloadRefusal
	// readErr is the first failure to read the cluster; after one, the
	// observer stops reading rather than failing every sweep the same way,
	// and the timeout names it.
	readErr error
}

func newWorkloadObserver(request *ObserveRequest) *workloadObserver {
	out := request.Output
	if out == nil {
		out = io.Discard
	}
	return &workloadObserver{kubeconfig: request.Kubeconfig, kubeContext: request.KubeContext, out: out, seen: map[string]bool{}}
}

// denied is every admission denial seen so far.
func (o *workloadObserver) denied() []workloadRefusal {
	var denied []workloadRefusal
	for i := range o.refusals {
		if o.refusals[i].Denied() {
			denied = append(denied, o.refusals[i])
		}
	}
	return denied
}

// inspect reads the controller events of every workload the applications
// deliver, prints the refusals not seen before, and returns them. A cluster
// that cannot be read is reported once and does not end the observation:
// Argo CD's own health is still awaited as before.
func (o *workloadObserver) inspect(ctx context.Context, apps []*argoApplication) []workloadRefusal {
	if o.readErr != nil {
		return nil
	}
	byNamespace := deliveredWorkloads(apps)
	namespaces := make([]string, 0, len(byNamespace))
	for namespace := range byNamespace {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	var fresh []workloadRefusal
	for _, namespace := range namespaces {
		refusals, err := o.namespaceRefusals(ctx, namespace, byNamespace[namespace])
		if err != nil {
			o.readErr = err
			fmt.Fprintf(o.out, "cannot read the delivered workloads' events: %v\n", err)
			return fresh
		}
		for _, refusal := range refusals {
			key := refusal.Workload.String() + "\x00" + refusal.Message
			if o.seen[key] {
				continue
			}
			o.seen[key] = true
			o.refusals = append(o.refusals, refusal)
			fresh = append(fresh, refusal)
			fmt.Fprintf(o.out, "%s\n", &refusal)
		}
	}
	return fresh
}

// deliveredWorkloads is the workloads the applications reconcile, by
// namespace, as Argo CD lists them, each with the moment its application's
// current sync began: an event recorded before it belongs to a previous
// rollout — a ReplicaSet the Deployment keeps in its history, a Job re-synced
// under the same name — and says nothing of this one. An application that
// reports no start leaves its workloads unbounded.
func deliveredWorkloads(apps []*argoApplication) map[string]map[deliveredWorkload]time.Time {
	byNamespace := map[string]map[deliveredWorkload]time.Time{}
	for _, app := range apps {
		since, _ := time.Parse(time.RFC3339, app.Status.OperationState.StartedAt)
		for _, resource := range app.Status.Resources {
			if !workloadKinds[resource.Kind] {
				continue
			}
			if byNamespace[resource.Namespace] == nil {
				byNamespace[resource.Namespace] = map[deliveredWorkload]time.Time{}
			}
			byNamespace[resource.Namespace][deliveredWorkload{Namespace: resource.Namespace, Kind: resource.Kind, Name: resource.Name}] = since
		}
	}
	return byNamespace
}

// kubeEvent is the part of a Kubernetes Event the refusal is read from.
type kubeEvent struct {
	Type           string `json:"type"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
	LastTimestamp string `json:"lastTimestamp"`
	EventTime     string `json:"eventTime"`
	Series        struct {
		LastObservedTime string `json:"lastObservedTime"`
	} `json:"series"`
}

// lastSeen is the latest moment the event was observed, over the three
// fields an Event carries it in; zero when it carries none that parses.
func (e *kubeEvent) lastSeen() time.Time {
	var latest time.Time
	for _, value := range []string{e.LastTimestamp, e.EventTime, e.Series.LastObservedTime} {
		if at, err := time.Parse(time.RFC3339Nano, value); err == nil && at.After(latest) {
			latest = at
		}
	}
	return latest
}

// kubeOwned is the part of a ReplicaSet that names the Deployment it serves.
type kubeOwned struct {
	Metadata struct {
		Name            string `json:"name"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
}

// namespaceRefusals reads one namespace's events and returns those recorded
// against a delivered workload's controller since its sync began: a
// FailedCreate, or a warning that is an admission denial. A Deployment's pods
// are created by its ReplicaSet, so the ReplicaSets are read too and each
// event on one is attributed to the Deployment that owns it. An event whose
// time cannot be read is kept: shown once too often rather than hidden.
func (o *workloadObserver) namespaceRefusals(ctx context.Context, namespace string, workloads map[deliveredWorkload]time.Time) ([]workloadRefusal, error) {
	var events struct {
		Items []kubeEvent `json:"items"`
	}
	if err := o.get(ctx, namespace, "events", &events); err != nil {
		return nil, err
	}
	owners := map[string]string{}
	for workload := range workloads {
		if workload.Kind != kindDeployment {
			continue
		}
		var replicaSets struct {
			Items []kubeOwned `json:"items"`
		}
		if err := o.get(ctx, namespace, "replicasets", &replicaSets); err != nil {
			return nil, err
		}
		for _, replicaSet := range replicaSets.Items {
			for _, owner := range replicaSet.Metadata.OwnerReferences {
				if owner.Kind == kindDeployment {
					owners[replicaSet.Metadata.Name] = owner.Name
				}
			}
		}
		break
	}
	var refusals []workloadRefusal
	for i := range events.Items {
		event := &events.Items[i]
		refusal := workloadRefusal{Reason: event.Reason, Message: event.Message}
		if event.Reason != "FailedCreate" && (event.Type != "Warning" || !refusal.Denied()) {
			continue
		}
		kind, name := event.InvolvedObject.Kind, event.InvolvedObject.Name
		if kind == kindReplicaSet {
			kind, name = kindDeployment, owners[name]
		}
		refusal.Workload = deliveredWorkload{Namespace: namespace, Kind: kind, Name: name}
		since, delivered := workloads[refusal.Workload]
		if !delivered {
			continue
		}
		if seen := event.lastSeen(); !since.IsZero() && !seen.IsZero() && seen.Before(since) {
			continue
		}
		refusals = append(refusals, refusal)
	}
	return refusals, nil
}

// get lists one resource kind of a namespace as JSON through kubectl, against
// the kubeconfig and context the environment declares; declaring neither
// reads kubectl's current context, as every other cluster call does.
func (o *workloadObserver) get(ctx context.Context, namespace, kind string, into any) error {
	var args []string
	if o.kubeconfig != "" {
		args = append(args, "--kubeconfig", o.kubeconfig)
	}
	if o.kubeContext != "" {
		args = append(args, "--context", o.kubeContext)
	}
	args = append(args, "get", kind, "-n", namespace, "-o", "json")
	output, err := command(ctx, "", "kubectl", args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(output), into); err != nil {
		return fmt.Errorf("decode %s of namespace %s: %w", kind, namespace, err)
	}
	return nil
}

// unhealthyResources names every resource the applications report as other
// than Healthy, with the message Argo CD attached, for the timeout to say
// which workload it waited on.
func unhealthyResources(apps []*argoApplication) []string {
	var described []string
	for _, app := range apps {
		for _, resource := range app.Status.Resources {
			if resource.Health.Status == "" || resource.Health.Status == healthyStatus {
				continue
			}
			line := fmt.Sprintf("%s/%s/%s health=%s", resource.Namespace, resource.Kind, resource.Name, resource.Health.Status)
			if resource.Health.Message != "" {
				line += ": " + resource.Health.Message
			}
			described = append(described, line)
		}
	}
	return described
}

// describeRefusals joins refusals for an error message.
func describeRefusals(refusals []workloadRefusal) string {
	described := make([]string, 0, len(refusals))
	for i := range refusals {
		described = append(described, refusals[i].String())
	}
	return strings.Join(described, "; ")
}
