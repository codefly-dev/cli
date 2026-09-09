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

// bootstrapKinds are the kinds that must run to completion for
// StageBootstrapped; rolloutKinds are the workloads that must finish rolling
// out for StageHealthy.
var (
	bootstrapKinds = map[string]bool{"Job": true}
	rolloutKinds   = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}
)

const (
	rolloutUnobserved = "rollout not observed by the controller yet"
	rolloutComplete   = "rollout complete"
)

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
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
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

// ownedResources lists the Jobs and workloads the rendered manifests own, so
// observation is bound to the exact deployment evidence rather than to whatever
// else shares the namespace.
func ownedResources(manifests string, env *resources.Environment) ([]ownedResource, error) {
	headers, err := manifestHeaders(manifests)
	if err != nil {
		return nil, err
	}
	var owned []ownedResource
	for _, header := range headers {
		if header.Metadata.Name == "" || (!bootstrapKinds[header.Kind] && !rolloutKinds[header.Kind]) {
			continue
		}
		namespace := header.Metadata.Namespace
		if namespace == "" {
			namespace = defaultNamespace(env)
		}
		owned = append(owned, ownedResource{kind: header.Kind, namespace: namespace, name: header.Metadata.Name})
	}
	sort.Slice(owned, func(i, j int) bool {
		if owned[i].kind != owned[j].kind {
			return owned[i].kind < owned[j].kind
		}
		if owned[i].namespace != owned[j].namespace {
			return owned[i].namespace < owned[j].namespace
		}
		return owned[i].name < owned[j].name
	})
	return owned, nil
}

func defaultNamespace(env *resources.Environment) string {
	if env != nil && env.Namespace != "" {
		return env.Namespace
	}
	return "default"
}

// partitionDocuments splits rendered documents into the preparation resources a
// rollout depends on — Jobs included — and the workload rollouts themselves.
// Position in the rendered stream carries no ordering meaning, so the split is
// by kind.
func partitionDocuments(documents []string) ([]string, []string, error) {
	var preparation, rollout []string
	for _, document := range documents {
		if strings.TrimSpace(document) == "" {
			continue
		}
		headers, err := manifestHeaders(document)
		if err != nil {
			return nil, nil, err
		}
		isRollout := false
		for _, header := range headers {
			if rolloutKinds[header.Kind] {
				isRollout = true
			}
		}
		if isRollout {
			rollout = append(rollout, document)
			continue
		}
		preparation = append(preparation, document)
	}
	return preparation, rollout, nil
}

// completionObserver watches the resources one applied tree owns on the exact
// verified target, re-verifying that target on every sweep.
type completionObserver struct {
	env    *resources.Environment
	target *VerifiedKubernetesTarget
}

// await blocks until every owned resource relevant to stage is ready, one of
// them fails terminally, the budget runs out, or the caller cancels. It always
// returns the last state it read so the caller can record it as evidence.
func (observer completionObserver) await(
	ctx context.Context,
	owned []ownedResource,
	stage CompletionStage,
	timeout time.Duration,
) ([]ObservedResource, error) {
	var watched []ownedResource
	for _, resource := range owned {
		if bootstrapKinds[resource.kind] || (stage.AtLeast(StageHealthy) && rolloutKinds[resource.kind]) {
			watched = append(watched, resource)
		}
	}
	if len(watched) == 0 {
		return nil, nil
	}
	if timeout <= 0 {
		timeout = DefaultCompletionTimeout
	}
	interval := observationInterval(timeout)
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last []ObservedResource
	for {
		kubeconfig, err := verifiedKubeconfigSnapshot(bounded, observer.env, observer.target)
		if err != nil {
			if bounded.Err() != nil {
				return last, observer.unfinished(ctx, stage, timeout, last)
			}
			return last, fmt.Errorf("cannot verify Kubernetes target before observation: %w", err)
		}
		current := make([]ObservedResource, 0, len(watched))
		var failures []string
		pending := 0
		for _, resource := range watched {
			observed := observer.observe(bounded, kubeconfig, resource)
			current = append(current, observed)
			switch observed.State {
			case ResourceFailed:
				failures = append(failures, observed.String())
			case ResourcePending:
				pending++
			}
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
			return last, observer.unfinished(ctx, stage, timeout, last)
		case <-timer.C:
		}
	}
}

func (observer completionObserver) unfinished(
	ctx context.Context,
	stage CompletionStage,
	timeout time.Duration,
	last []ObservedResource,
) error {
	var reasons []string
	for _, observed := range last {
		if observed.State != ResourceReady {
			reasons = append(reasons, observed.String())
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
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas    *int32 `json:"replicas"`
		Completions *int32 `json:"completions"`
		Selector    struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
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

func (observer completionObserver) observe(ctx context.Context, kubeconfig []byte, resource ownedResource) ObservedResource {
	observed := ObservedResource{Kind: resource.kind, Namespace: resource.namespace, Name: resource.name}
	raw, err := runKubectl(ctx, observer.target, kubeconfig, "",
		"get", strings.ToLower(resource.kind), resource.name, "--namespace", resource.namespace, "-o", "json")
	if err != nil {
		observed.State = ResourcePending
		observed.Message = "not readable yet: " + err.Error()
		return observed
	}
	var document workloadDocument
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		observed.State = ResourcePending
		observed.Message = fmt.Sprintf("cannot decode status: %v", err)
		return observed
	}

	switch resource.kind {
	case "Job":
		observed.State, observed.Message = jobState(&document)
	case "Deployment":
		observed.State, observed.Message = deploymentState(&document)
	case "StatefulSet":
		observed.State, observed.Message = statefulSetState(&document)
	case "DaemonSet":
		observed.State, observed.Message = daemonSetState(&document)
	}
	if observed.State != ResourcePending {
		return observed
	}
	if state, message, found := observer.podDiagnostic(ctx, kubeconfig, resource, document.Spec.Selector.MatchLabels); found {
		observed.State = state
		observed.Message = observed.Message + "; " + message
	}
	return observed
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
	for _, condition := range document.Status.Conditions {
		if condition.Type == "Progressing" && condition.Status == "False" {
			return ResourceFailed, "rollout stopped progressing: " + conditionDetail(condition)
		}
		if condition.Type == "ReplicaFailure" && condition.Status == "True" {
			return ResourceFailed, "replicas cannot be created: " + conditionDetail(condition)
		}
	}
	desired := desiredReplicas(document)
	status := document.Status
	if status.UpdatedReplicas == desired && status.ReadyReplicas == desired && status.Replicas == desired {
		return ResourceReady, rolloutComplete
	}
	return ResourcePending, fmt.Sprintf(
		"%d/%d replicas ready, %d updated",
		status.ReadyReplicas, desired, status.UpdatedReplicas,
	)
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

type podContainerStatus struct {
	Name  string `json:"name"`
	State struct {
		Waiting struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
	} `json:"state"`
}

type podListDocument struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			ContainerStatuses     []podContainerStatus `json:"containerStatuses"`
			InitContainerStatuses []podContainerStatus `json:"initContainerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// podDiagnostic explains why an owned workload is still pending, using only the
// pods that workload's own selector matches. An image the cluster cannot pull
// is terminal: no amount of waiting fixes it.
func (observer completionObserver) podDiagnostic(
	ctx context.Context,
	kubeconfig []byte,
	resource ownedResource,
	matchLabels map[string]string,
) (ResourceState, string, bool) {
	if len(matchLabels) == 0 {
		return "", "", false
	}
	pairs := make([]string, 0, len(matchLabels))
	for key, value := range matchLabels {
		pairs = append(pairs, key+"="+value)
	}
	sort.Strings(pairs)
	raw, err := runKubectl(ctx, observer.target, kubeconfig, "",
		"get", "pods", "--namespace", resource.namespace, "--selector", strings.Join(pairs, ","), "-o", "json")
	if err != nil {
		return "", "", false
	}
	var pods podListDocument
	if err := json.Unmarshal([]byte(raw), &pods); err != nil {
		return "", "", false
	}
	for _, pod := range pods.Items {
		for _, container := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			waiting := container.State.Waiting
			if waiting.Reason == "" {
				continue
			}
			detail := fmt.Sprintf("pod %s container %s is %s", pod.Metadata.Name, container.Name, waiting.Reason)
			if waiting.Message != "" {
				detail += ": " + waiting.Message
			}
			if terminalWaitingReasons[waiting.Reason] {
				return ResourceFailed, detail, true
			}
			return ResourcePending, detail, true
		}
	}
	return "", "", false
}
