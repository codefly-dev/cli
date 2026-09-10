package deployments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// ResourceState is the state observation last read for one owned resource.
type ResourceState string

const (
	ResourcePending ResourceState = "pending"
	ResourceReady   ResourceState = "ready"
	ResourceFailed  ResourceState = "failed"
)

// ObservedResource is one resource the applied manifests own, with the state
// the cluster last reported for it. Only kind, identity and the cluster's own
// status reason and message are carried — never manifest content.
type ObservedResource struct {
	Kind      string
	Namespace string
	Name      string
	State     ResourceState
	Message   string
}

func (resource ObservedResource) String() string {
	return fmt.Sprintf("%s %s/%s: %s", resource.Kind, resource.Namespace, resource.Name, resource.Message)
}

const (
	kindJob         = "Job"
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
)

// bootstrapKinds are the kinds that must run to completion for
// StageBootstrapped; rolloutKinds are the workloads that must finish rolling
// out for StageHealthy.
var (
	bootstrapKinds = map[string]bool{kindJob: true}
	rolloutKinds   = map[string]bool{kindDeployment: true, kindStatefulSet: true, kindDaemonSet: true}
)

// bootstrapJobLabel marks a Job as schema preparation for a service. Only a Job
// carrying it forms the barrier a consumer rollout waits behind; an ordinary Job
// — a smoke test, a post-deploy verification that talks to its own Service —
// keeps its rendered position after the workloads it exercises.
const bootstrapJobLabel = "codefly.dev/bootstrap-service"

const (
	rolloutUnobserved = "rollout not observed by the controller yet"
	rolloutComplete   = "rollout complete"
)

// unreadableSweepGrace is how many consecutive sweeps a resource may fail to
// report before observation calls it failed instead of waiting out the budget.
// An owned resource was applied before observation started, so a persistent
// NotFound means it is gone, not that it has yet to appear.
const unreadableSweepGrace = 3

// terminalWaitingReasons are container waiting reasons a rollout never recovers
// from on its own, so observation reports them instead of burning the timeout.
var terminalWaitingReasons = map[string]bool{
	"ImagePullBackOff": true,
	"InvalidImageName": true,
}

// ownedResource identifies one resource the applied evidence owns. Observation
// never reads anything outside this set.
type ownedResource struct {
	kind      string
	namespace string
	name      string
}

type manifestHeader struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
}

func manifestHeaders(manifests string) ([]manifestHeader, error) {
	decoder := yaml.NewDecoder(strings.NewReader(manifests))
	var headers []manifestHeader
	for {
		var header manifestHeader
		err := decoder.Decode(&header)
		if errors.Is(err, io.EOF) {
			return headers, nil
		}
		if err != nil {
			return nil, fmt.Errorf("cannot decode rendered manifest: %w", err)
		}
		headers = append(headers, header)
	}
}

func isBootstrapJob(header *manifestHeader) bool {
	return header.Kind == kindJob && header.Metadata.Labels[bootstrapJobLabel] != ""
}

func ownedResourceOf(header *manifestHeader, namespace string) (ownedResource, bool) {
	if header.Metadata.Name == "" || (!bootstrapKinds[header.Kind] && !rolloutKinds[header.Kind]) {
		return ownedResource{}, false
	}
	declared := header.Metadata.Namespace
	if declared == "" {
		declared = namespace
	}
	return ownedResource{kind: header.Kind, namespace: declared, name: header.Metadata.Name}, true
}

func sortOwned(owned []ownedResource) {
	sort.Slice(owned, func(i, j int) bool {
		if owned[i].kind != owned[j].kind {
			return owned[i].kind < owned[j].kind
		}
		if owned[i].namespace != owned[j].namespace {
			return owned[i].namespace < owned[j].namespace
		}
		return owned[i].name < owned[j].name
	})
}

// applyPlan is the apply order and the observation targets for one rendered
// tree, derived from a single walk so the two can never disagree about what a
// phase owns.
type applyPlan struct {
	// Preparation is applied before Rollout. Without a barrier every document
	// stays in Preparation in its rendered order, so a caller that only requires
	// StageApplied gets exactly the order kustomize emitted.
	Preparation []string
	Rollout     []string
	// BootstrapJobs are the Jobs in Preparation: the barrier waits on these and
	// never reads them again. RolloutTargets are the workloads and ordinary Jobs
	// in Rollout, observed for health.
	BootstrapJobs  []ownedResource
	RolloutTargets []ownedResource
}

// planApply splits rendered documents into the schema preparation a rollout
// waits behind and the rollout itself.
//
// barrier is the caller's requested completion, not a property of the tree.
// Reordering documents changes which resources exist when, so a caller that
// asked only for StageApplied must get its manifests applied exactly as
// rendered. Position is overridden only when a barrier was asked for, and then
// only for the bootstrap Jobs that are the barrier: an ordinary Job stays with
// the rollout, because a post-deploy verification Job hoisted ahead of the
// Deployment it exercises would fail, or deadlock the barrier waiting for it.
func planApply(documents []string, namespace string, barrier bool) (applyPlan, error) {
	var plan applyPlan
	for _, document := range documents {
		if strings.TrimSpace(document) == "" {
			continue
		}
		headers, err := manifestHeaders(document)
		if err != nil {
			return applyPlan{}, err
		}
		if !barrier {
			plan.Preparation = append(plan.Preparation, document)
			continue
		}
		rollout := false
		for index := range headers {
			header := &headers[index]
			if rolloutKinds[header.Kind] || (header.Kind == kindJob && !isBootstrapJob(header)) {
				rollout = true
			}
		}
		if rollout {
			plan.Rollout = append(plan.Rollout, document)
		} else {
			plan.Preparation = append(plan.Preparation, document)
		}
		for index := range headers {
			resource, owned := ownedResourceOf(&headers[index], namespace)
			if !owned {
				continue
			}
			if rollout {
				plan.RolloutTargets = append(plan.RolloutTargets, resource)
			} else {
				plan.BootstrapJobs = append(plan.BootstrapJobs, resource)
			}
		}
	}
	sortOwned(plan.BootstrapJobs)
	sortOwned(plan.RolloutTargets)
	return plan, nil
}

// ownedResources lists every Job and workload the rendered manifests own, so a
// caller observing a tree it applied elsewhere is bound to the same evidence.
func ownedResources(manifests, namespace string) ([]ownedResource, error) {
	plan, err := planApply([]string{manifests}, namespace, true)
	if err != nil {
		return nil, err
	}
	owned := make([]ownedResource, 0, len(plan.BootstrapJobs)+len(plan.RolloutTargets))
	owned = append(owned, plan.BootstrapJobs...)
	owned = append(owned, plan.RolloutTargets...)
	sortOwned(owned)
	return owned, nil
}

// completionObserver watches the resources one applied tree owns on the exact
// verified target, re-verifying that target on every sweep.
type completionObserver struct {
	env    *resources.Environment
	target *VerifiedKubernetesTarget
}

// await blocks until every watched resource is ready, one of them fails
// terminally, the budget runs out, or the caller cancels. The caller passes the
// exact set to watch: a resource whose stage is already established is never
// handed back here, because re-reading it can only produce a false negative.
func (observer completionObserver) await(
	ctx context.Context,
	watched []ownedResource,
	stage CompletionStage,
	timeout time.Duration,
) ([]ObservedResource, error) {
	if len(watched) == 0 {
		return nil, nil
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("no observation budget left to establish %s", stage)
	}
	interval := observationInterval(timeout)
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// A resource that reached its stage is settled and never read again: a Job
	// with ttlSecondsAfterFinished disappears shortly after completing, and a
	// re-read would report the deployment that just succeeded as missing.
	settled := make(map[ownedResource]ObservedResource, len(watched))
	unreadable := make(map[ownedResource]int, len(watched))
	var last []ObservedResource
	for {
		kubeconfig, err := verifiedKubeconfigSnapshot(bounded, observer.env, observer.target)
		if err != nil {
			if bounded.Err() != nil {
				return last, observer.unfinished(ctx, stage, timeout, watched, last)
			}
			return last, fmt.Errorf("cannot verify Kubernetes target before observation: %w", err)
		}
		current := make([]ObservedResource, 0, len(watched))
		var failures []string
		pending := 0
		torn := false
		for _, resource := range watched {
			if bounded.Err() != nil {
				torn = true
				break
			}
			if done, established := settled[resource]; established {
				current = append(current, done)
				continue
			}
			observed, readable := observer.observe(bounded, kubeconfig, resource)
			if !readable && bounded.Err() != nil {
				// Our own deadline killed the read. It says nothing about the
				// resource, so it must not count against the grace below.
				torn = true
				break
			}
			if readable {
				delete(unreadable, resource)
			} else {
				unreadable[resource]++
				if unreadable[resource] > unreadableSweepGrace {
					observed.State = ResourceFailed
					observed.Message = "applied but the target no longer reports it: " + observed.Message
				}
			}
			if observed.State == ResourceReady {
				settled[resource] = observed
			}
			current = append(current, observed)
			switch observed.State {
			case ResourceFailed:
				failures = append(failures, observed.String())
			case ResourcePending:
				pending++
			}
		}
		if torn {
			// The budget expired partway through this sweep. Report the last
			// complete one when there is one: "0/1 completions" tells an
			// operator what to look at, "context deadline exceeded" tells them
			// only that we stopped waiting.
			return last, observer.unfinished(ctx, stage, timeout, watched, last)
		}
		last = current
		if len(failures) > 0 {
			return last, fmt.Errorf("deployment cannot reach %s: %s", stage, strings.Join(failures, "; "))
		}
		if pending == 0 {
			return last, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-bounded.Done():
			timer.Stop()
			return last, observer.unfinished(ctx, stage, timeout, watched, last)
		case <-timer.C:
		}
	}
}

// unfinished names what the deployment was still waiting on. It falls back to
// the watched set when the budget expired before any resource could be read, so
// the error always says which resources were outstanding rather than trailing
// off after the colon.
func (observer completionObserver) unfinished(
	ctx context.Context,
	stage CompletionStage,
	timeout time.Duration,
	watched []ownedResource,
	last []ObservedResource,
) error {
	var reasons []string
	for _, observed := range last {
		if observed.State != ResourceReady {
			reasons = append(reasons, observed.String())
		}
	}
	if len(reasons) == 0 {
		for _, resource := range watched {
			reasons = append(reasons, fmt.Sprintf(
				"%s %s/%s: no status was read before the budget expired",
				resource.kind, resource.namespace, resource.name,
			))
		}
	}
	detail := strings.Join(reasons, "; ")
	if ctx.Err() != nil {
		return fmt.Errorf("deployment observation cancelled before reaching %s: %s", stage, detail)
	}
	return fmt.Errorf("deployment did not reach %s within %s: %s", stage, timeout, detail)
}

// observationInterval keeps a long budget from polling the API server hard
// while still giving a short one several sweeps before it expires.
func observationInterval(timeout time.Duration) time.Duration {
	interval := timeout / 20
	if interval > 2*time.Second {
		return 2 * time.Second
	}
	if interval < 50*time.Millisecond {
		return 50 * time.Millisecond
	}
	return interval
}

type statusCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// workloadDocument covers the status fields of every observed kind; a field a
// given kind does not carry simply decodes to its zero value.
type workloadDocument struct {
	Metadata struct {
		Generation int64  `json:"generation"`
		UID        string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		Replicas    *int32 `json:"replicas"`
		Completions *int32 `json:"completions"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration     int64             `json:"observedGeneration"`
		Replicas               int32             `json:"replicas"`
		ReadyReplicas          int32             `json:"readyReplicas"`
		UpdatedReplicas        int32             `json:"updatedReplicas"`
		CurrentRevision        string            `json:"currentRevision"`
		UpdateRevision         string            `json:"updateRevision"`
		DesiredNumberScheduled int32             `json:"desiredNumberScheduled"`
		NumberReady            int32             `json:"numberReady"`
		UpdatedNumberScheduled int32             `json:"updatedNumberScheduled"`
		Succeeded              int32             `json:"succeeded"`
		Failed                 int32             `json:"failed"`
		Conditions             []statusCondition `json:"conditions"`
	} `json:"status"`
}

func (document *workloadDocument) condition(kind string) (statusCondition, bool) {
	for _, condition := range document.Status.Conditions {
		if condition.Type == kind && condition.Status == "True" {
			return condition, true
		}
	}
	return statusCondition{}, false
}

func conditionDetail(condition statusCondition) string {
	detail := condition.Reason
	if condition.Message != "" {
		if detail == "" {
			return condition.Message
		}
		return detail + ": " + condition.Message
	}
	return detail
}

// observe reads one owned resource. The second return reports whether a usable
// status came back at all, which is what separates "still settling" from "gone".
func (observer completionObserver) observe(
	ctx context.Context,
	kubeconfig []byte,
	resource ownedResource,
) (ObservedResource, bool) {
	observed := ObservedResource{Kind: resource.kind, Namespace: resource.namespace, Name: resource.name}
	raw, err := runKubectl(ctx, observer.target, kubeconfig, "",
		"get", strings.ToLower(resource.kind), resource.name, "--namespace", resource.namespace, "-o", "json")
	if err != nil {
		observed.State = ResourcePending
		observed.Message = "not readable: " + err.Error()
		return observed, false
	}
	var document workloadDocument
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		observed.State = ResourcePending
		observed.Message = fmt.Sprintf("cannot decode status: %v", err)
		return observed, false
	}

	switch resource.kind {
	case kindJob:
		observed.State, observed.Message = jobState(&document)
	case kindDeployment:
		observed.State, observed.Message = deploymentState(&document)
	case kindStatefulSet:
		observed.State, observed.Message = statefulSetState(&document)
	case kindDaemonSet:
		observed.State, observed.Message = daemonSetState(&document)
	}
	if observed.State != ResourcePending {
		return observed, true
	}
	if state, message, found := observer.podDiagnostic(ctx, kubeconfig, resource, &document); found {
		observed.State = state
		observed.Message = observed.Message + "; " + message
	}
	return observed, true
}

func jobState(document *workloadDocument) (ResourceState, string) {
	if condition, failed := document.condition("Failed"); failed {
		return ResourceFailed, "Job failed: " + conditionDetail(condition)
	}
	if _, complete := document.condition("Complete"); complete {
		return ResourceReady, "completed"
	}
	completions := int32(1)
	if document.Spec.Completions != nil {
		completions = *document.Spec.Completions
	}
	return ResourcePending, fmt.Sprintf(
		"%d/%d completions, %d failed pods",
		document.Status.Succeeded, completions, document.Status.Failed,
	)
}

func deploymentState(document *workloadDocument) (ResourceState, string) {
	if document.Status.ObservedGeneration < document.Metadata.Generation {
		return ResourcePending, rolloutUnobserved
	}
	desired := desiredReplicas(document)
	status := document.Status
	if status.UpdatedReplicas == desired && status.ReadyReplicas == desired && status.Replicas == desired {
		return ResourceReady, rolloutComplete
	}
	message := fmt.Sprintf("%d/%d replicas ready, %d updated", status.ReadyReplicas, desired, status.UpdatedReplicas)
	// Progressing=False (ProgressDeadlineExceeded) and ReplicaFailure are not
	// terminal: the Deployment controller keeps reconciling, and both clear on
	// their own once the slow image pull or the exhausted quota that caused them
	// resolves. They explain a pending rollout; the caller's budget bounds it.
	for _, condition := range status.Conditions {
		if (condition.Type == "Progressing" && condition.Status == "False") ||
			(condition.Type == "ReplicaFailure" && condition.Status == "True") {
			message += "; " + condition.Type + " " + conditionDetail(condition)
		}
	}
	return ResourcePending, message
}

func statefulSetState(document *workloadDocument) (ResourceState, string) {
	if document.Status.ObservedGeneration < document.Metadata.Generation {
		return ResourcePending, rolloutUnobserved
	}
	desired := desiredReplicas(document)
	status := document.Status
	if status.ReadyReplicas == desired && status.UpdatedReplicas == desired &&
		(status.UpdateRevision == "" || status.CurrentRevision == status.UpdateRevision) {
		return ResourceReady, rolloutComplete
	}
	return ResourcePending, fmt.Sprintf(
		"%d/%d replicas ready, %d updated",
		status.ReadyReplicas, desired, status.UpdatedReplicas,
	)
}

func daemonSetState(document *workloadDocument) (ResourceState, string) {
	if document.Status.ObservedGeneration < document.Metadata.Generation {
		return ResourcePending, rolloutUnobserved
	}
	status := document.Status
	if status.NumberReady == status.DesiredNumberScheduled &&
		status.UpdatedNumberScheduled == status.DesiredNumberScheduled {
		return ResourceReady, rolloutComplete
	}
	return ResourcePending, fmt.Sprintf(
		"%d/%d nodes ready, %d updated",
		status.NumberReady, status.DesiredNumberScheduled, status.UpdatedNumberScheduled,
	)
}

func desiredReplicas(document *workloadDocument) int32 {
	if document.Spec.Replicas != nil {
		return *document.Spec.Replicas
	}
	return 1
}

type ownerReference struct {
	UID string `json:"uid"`
}

type podContainerStatus struct {
	Name  string `json:"name"`
	State struct {
		Waiting struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
	} `json:"state"`
}

type podDocument struct {
	Metadata struct {
		Name            string           `json:"name"`
		OwnerReferences []ownerReference `json:"ownerReferences"`
	} `json:"metadata"`
	Status struct {
		ContainerStatuses     []podContainerStatus `json:"containerStatuses"`
		InitContainerStatuses []podContainerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type podListDocument struct {
	Items []podDocument `json:"items"`
}

// controllerListDocument is the ownership shape of an intermediate controller,
// currently the ReplicaSets a Deployment owns.
type controllerListDocument struct {
	Items []struct {
		Metadata struct {
			UID             string           `json:"uid"`
			OwnerReferences []ownerReference `json:"ownerReferences"`
		} `json:"metadata"`
	} `json:"items"`
}

// podDiagnostic explains why an owned workload is still pending, using only the
// pods that workload actually owns.
//
// Ownership is the ownerReferences chain, never the label selector: kustomize
// commonLabels routinely stamp a workload's selector labels onto resources it
// does not own, and a sibling Job's failing pod would then be reported as this
// Deployment's failure — exactly the cross-talk that binding observation to the
// applied evidence is meant to prevent.
func (observer completionObserver) podDiagnostic(
	ctx context.Context,
	kubeconfig []byte,
	resource ownedResource,
	document *workloadDocument,
) (ResourceState, string, bool) {
	if document.Metadata.UID == "" {
		return "", "", false
	}
	owners := observer.podOwners(ctx, kubeconfig, resource, document.Metadata.UID)
	raw, err := runKubectl(ctx, observer.target, kubeconfig, "",
		"get", "pods", "--namespace", resource.namespace, "-o", "json")
	if err != nil {
		return "", "", false
	}
	var pods podListDocument
	if err := json.Unmarshal([]byte(raw), &pods); err != nil {
		return "", "", false
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !ownedBy(pod.Metadata.OwnerReferences, owners) {
			continue
		}
		for _, container := range pod.Status.InitContainerStatuses {
			if state, message, found := containerDiagnostic(pod.Metadata.Name, container); found {
				return state, message, true
			}
		}
		for _, container := range pod.Status.ContainerStatuses {
			if state, message, found := containerDiagnostic(pod.Metadata.Name, container); found {
				return state, message, true
			}
		}
	}
	return "", "", false
}

// podOwners is the set of uids whose pods belong to this workload. A Deployment
// owns its pods through the ReplicaSets it owns, so its own uid matches nothing.
func (observer completionObserver) podOwners(
	ctx context.Context,
	kubeconfig []byte,
	resource ownedResource,
	uid string,
) map[string]bool {
	owners := map[string]bool{uid: true}
	if resource.kind != kindDeployment {
		return owners
	}
	raw, err := runKubectl(ctx, observer.target, kubeconfig, "",
		"get", "replicasets", "--namespace", resource.namespace, "-o", "json")
	if err != nil {
		return owners
	}
	var sets controllerListDocument
	if err := json.Unmarshal([]byte(raw), &sets); err != nil {
		return owners
	}
	for _, set := range sets.Items {
		for _, reference := range set.Metadata.OwnerReferences {
			if reference.UID == uid {
				owners[set.Metadata.UID] = true
			}
		}
	}
	return owners
}

func ownedBy(references []ownerReference, owners map[string]bool) bool {
	for _, reference := range references {
		if owners[reference.UID] {
			return true
		}
	}
	return false
}

func containerDiagnostic(pod string, container podContainerStatus) (ResourceState, string, bool) {
	waiting := container.State.Waiting
	if waiting.Reason == "" {
		return "", "", false
	}
	detail := fmt.Sprintf("pod %s container %s is %s", pod, container.Name, waiting.Reason)
	if waiting.Message != "" {
		detail += ": " + waiting.Message
	}
	if terminalWaitingReasons[waiting.Reason] {
		return ResourceFailed, detail, true
	}
	return ResourcePending, detail, true
}
