package localcluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
)

// What a run's delivery IS: a Job attempt this run caused. A delivery Job is
// named after the carriers it posts, so an unchanged set keeps a Job an
// earlier run created — finished, deleted, or still there. The run therefore
// records the attempt that exists before it delivers, acquires a sync of each
// delivery Application identified by a nonce of its own (and only of the
// Application the CLI rendered, held to its full identity), and reads the
// attempt created by that sync.

// attemptKey names a delivery Job by namespace and name.
func attemptKey(job *gitops.DeliveryJob) string { return job.Namespace + "/" + job.Name }

// recordPriorAttempts reads the UID of each delivery Job that exists before
// this run delivers anything.
func (d *ClusterDriver) recordPriorAttempts(ctx context.Context, jobs []gitops.DeliveryJob) error {
	d.priorAttempts = map[string]string{}
	for index := range jobs {
		job := &jobs[index]
		if job.Application.Name == "" {
			return fmt.Errorf("delivery Job %s names no Argo CD Application the CLI rendered, so this run cannot ask for its delivery", attemptKey(job))
		}
		attempt, err := d.jobAttempt(ctx, job)
		if err != nil {
			return err
		}
		d.priorAttempts[attemptKey(job)] = attempt.uid
	}
	return nil
}

// acquireAttempts acquires one fresh sync of every delivery Application —
// always, whether its Job exists, finished, or was deleted: re-applying an
// unchanged bootstrap starts no sync, and a Sync hook runs only on one — and
// records when each started.
func (d *ClusterDriver) acquireAttempts(ctx context.Context, jobs []gitops.DeliveryJob) error {
	if d.nonce == "" {
		random := make([]byte, 8)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		d.nonce = hex.EncodeToString(random)
	}
	d.attemptStarts = map[string]time.Time{}
	d.attemptNonces = map[string]string{}
	started := map[string]time.Time{}
	applications := map[string]gitops.DeliveryApplication{}
	for index := range jobs {
		job := &jobs[index]
		at, acquired := started[job.Application.Name]
		if acquired && applications[job.Application.Name] != job.Application {
			return fmt.Errorf("ambiguous delivery Application %s: conflicting rendered identities", job.Application.Name)
		}
		if !acquired {
			var err error
			if at, err = d.acquireSync(ctx, &job.Application); err != nil {
				return err
			}
			started[job.Application.Name] = at
			applications[job.Application.Name] = job.Application
		}
		d.attemptStarts[attemptKey(job)] = at
		d.attemptNonces[attemptKey(job)] = d.nonce + "-" + job.Application.Name
	}
	return nil
}

// applicationState is the part of an Argo CD Application the run reads.
type applicationState struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		OwnerReferences []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			UID        string `json:"uid"`
			Controller bool   `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Sources []json.RawMessage `json:"sources"`
		Source  struct {
			RepoURL        string `json:"repoURL"`
			TargetRevision string `json:"targetRevision"`
			Path           string `json:"path"`
		} `json:"source"`
		Destination struct {
			Server    string `json:"server"`
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
	Operation json.RawMessage `json:"operation"`
	Status    struct {
		OperationState *struct {
			Phase     string `json:"phase"`
			StartedAt string `json:"startedAt"`
			Operation struct {
				Sync struct {
					Source struct {
						Kustomize struct {
							CommonAnnotations map[string]string `json:"commonAnnotations"`
						} `json:"kustomize"`
					} `json:"source"`
				} `json:"sync"`
				Info []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"info"`
			} `json:"operation"`
		} `json:"operationState"`
	} `json:"status"`
}

// mismatch names the first part of the Application's identity that is not
// the one the CLI rendered, or "".
func (state *applicationState) mismatch(want *gitops.DeliveryApplication) string {
	if mismatch := state.ownerMismatch(&want.Owner); mismatch != "" {
		return mismatch
	}
	source, destination := &state.Spec.Source, &state.Spec.Destination
	switch {
	case len(state.Spec.Sources) != 0:
		return "spec.sources overrides the rendered singular source"
	case source.RepoURL != want.RepoURL:
		return fmt.Sprintf("it syncs %q, not %q", source.RepoURL, want.RepoURL)
	case source.TargetRevision != want.TargetRevision:
		return fmt.Sprintf("it syncs revision %q, not %q", source.TargetRevision, want.TargetRevision)
	case source.Path != want.Path:
		return fmt.Sprintf("it syncs path %q, not %q", source.Path, want.Path)
	case destination.Server != want.Server:
		return fmt.Sprintf("it deploys to %q, not the in-cluster %q", destination.Server, want.Server)
	case destination.Namespace != want.Namespace:
		return fmt.Sprintf("it deploys to namespace %q, not %q", destination.Namespace, want.Namespace)
	}
	return ""
}

// ownerMismatch requires the controller that the bootstrap apply returned.
func (state *applicationState) ownerMismatch(want *gitops.ApplicationSetIdentity) string {
	if want.Kind != "ApplicationSet" || want.Name == "" || want.UID == "" || want.APIVersion == "" || want.Namespace != "argocd" {
		return "the rendered ApplicationSet has no bound identity from bootstrap apply"
	}
	if len(state.Metadata.OwnerReferences) != 1 {
		return "it must have exactly one ApplicationSet owner"
	}
	owner := state.Metadata.OwnerReferences[0]
	if !owner.Controller || owner.APIVersion != want.APIVersion || owner.Kind != want.Kind || owner.Name != want.Name || owner.UID != want.UID {
		return "its controller is not the ApplicationSet applied by this run"
	}
	return ""
}

// acceptedNonce is the attempt nonce of the operation Argo CD is running or
// ran last, or "".
func (state *applicationState) acceptedNonce() string {
	if state.Status.OperationState == nil {
		return ""
	}
	for _, info := range state.Status.OperationState.Operation.Info {
		if info.Name == attemptInfo {
			if state.Status.OperationState.Operation.Sync.Source.Kustomize.CommonAnnotations[deliveryAttemptAnnotation] == info.Value {
				return info.Value
			}
		}
	}
	return ""
}

// idle reports an Application with no operation requested or running.
func (state *applicationState) idle() bool {
	requested := len(state.Operation) > 0 && string(state.Operation) != "null"
	running := state.Status.OperationState != nil && (state.Status.OperationState.Phase == "Running" || state.Status.OperationState.Phase == "Terminating")
	return !requested && !running
}

const attemptInfo = "codefly-attempt"
const deliveryAttemptAnnotation = "codefly.dev/delivery-attempt"

// syncOperation stamps the nonce through Argo's per-operation source override.
// Delivery overlays are Kustomizations; the override annotates the hook itself
// before creation without mutating the Application or the published carriers.
func syncOperation(want *gitops.DeliveryApplication, nonce string) map[string]any {
	return map[string]any{
		"revision": want.TargetRevision,
		"source": map[string]any{
			"repoURL": want.RepoURL, "targetRevision": want.TargetRevision, "path": want.Path,
			"kustomize": map[string]any{
				"commonAnnotations":      map[string]string{deliveryAttemptAnnotation: nonce},
				"forceCommonAnnotations": true,
			},
		},
	}
}

// acquireSync waits for the Application the CLI rendered to exist with its
// full identity and an idle operation slot, requests a sync carrying this
// run's nonce — conditionally on the version it read, so a request that
// races another writer is refused rather than merged into its operation —
// and waits until Argo CD's operation state carries that nonce. A request
// another operation consumed is requested again. It returns when the
// accepted operation started.
func (d *ClusterDriver) acquireSync(ctx context.Context, want *gitops.DeliveryApplication) (time.Time, error) {
	nonce := d.nonce + "-" + want.Name
	deadline := time.Now().Add(d.AwaitTimeout)
	requested := false
	var last string
	for {
		state, found, err := d.application(ctx, want.Name)
		if err != nil {
			return time.Time{}, err
		}
		switch {
		case !found:
			last = "it does not exist yet"
		case state.mismatch(want) != "":
			last = state.mismatch(want)
		case state.acceptedNonce() == nonce:
			started, parseErr := time.Parse(time.RFC3339, state.Status.OperationState.StartedAt)
			if parseErr != nil {
				return time.Time{}, fmt.Errorf("the Application %s accepted this run's sync with an unreadable start %q: %w", want.Name, state.Status.OperationState.StartedAt, parseErr)
			}
			return started, nil
		case !state.idle():
			last = "another operation holds it"
			requested = false
		case !requested:
			operation, marshalErr := json.Marshal(map[string]any{
				keyMetadata: map[string]any{"resourceVersion": state.Metadata.ResourceVersion},
				"operation": map[string]any{
					"initiatedBy": map[string]any{"username": "codefly-run-cluster"},
					"info":        []any{map[string]string{"name": attemptInfo, "value": nonce}},
					"sync":        syncOperation(want, nonce),
				},
			})
			if marshalErr != nil {
				return time.Time{}, marshalErr
			}
			if _, patchErr := d.Kubectl(ctx, "", "-n", "argocd", "patch", "application", want.Name, "--type", "merge", "-p", string(operation)); patchErr != nil {
				last = "the request raced another writer: " + patchErr.Error()
			} else {
				requested, last = true, "Argo CD has not taken the request yet"
			}
		default:
			// Requested, the slot idle again and the nonce not accepted:
			// the request was consumed by something else. Ask again.
			requested = false
			continue
		}
		if time.Now().After(deadline) {
			return time.Time{}, fmt.Errorf("could not acquire a sync of Application %s after %s: %s", want.Name, d.AwaitTimeout, last)
		}
		if err = d.pause(ctx); err != nil {
			return time.Time{}, err
		}
	}
}

// application reads one Application by name.
func (d *ClusterDriver) application(ctx context.Context, name string) (*applicationState, bool, error) {
	listed, err := d.Kubectl(ctx, "", "-n", "argocd", "get", "applications", "--field-selector", "metadata.name="+name, "-o", "json")
	if err != nil {
		return nil, false, fmt.Errorf("read Application %s: %w", name, err)
	}
	var applications struct {
		Items []applicationState `json:"items"`
	}
	if err = json.Unmarshal([]byte(listed), &applications); err != nil {
		return nil, false, fmt.Errorf("read Application %s: %w", name, err)
	}
	switch len(applications.Items) {
	case 0:
		return nil, false, nil
	case 1:
		return &applications.Items[0], true, nil
	}
	return nil, false, fmt.Errorf("%d Applications are named %s", len(applications.Items), name)
}

// jobAttempt is one attempt of a delivery Job: its UID, when it was created
// and, once finished, whether it completed.
type jobAttempt struct {
	uid      string
	nonce    string
	created  time.Time
	finished bool
	complete bool
}

func (d *ClusterDriver) jobAttempt(ctx context.Context, job *gitops.DeliveryJob) (jobAttempt, error) {
	listed, err := d.Kubectl(ctx, "", "-n", job.Namespace, "get", "jobs", "--field-selector", "metadata.name="+job.Name, "-o", "json")
	if err != nil {
		return jobAttempt{}, fmt.Errorf("read delivery Job %s: %w", attemptKey(job), err)
	}
	var jobs struct {
		Items []struct {
			Metadata struct {
				UID               string            `json:"uid"`
				CreationTimestamp string            `json:"creationTimestamp"`
				Annotations       map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err = json.Unmarshal([]byte(listed), &jobs); err != nil {
		return jobAttempt{}, fmt.Errorf("read delivery Job %s: %w", attemptKey(job), err)
	}
	if len(jobs.Items) == 0 {
		return jobAttempt{}, nil
	}
	if len(jobs.Items) != 1 {
		return jobAttempt{}, fmt.Errorf("ambiguous delivery Job %s: %d matches", attemptKey(job), len(jobs.Items))
	}
	item := &jobs.Items[0]
	attempt := jobAttempt{uid: item.Metadata.UID, nonce: item.Metadata.Annotations[deliveryAttemptAnnotation]}
	attempt.created, _ = time.Parse(time.RFC3339, item.Metadata.CreationTimestamp)
	for _, condition := range item.Status.Conditions {
		if condition.Status == "True" && (condition.Type == "Complete" || condition.Type == "Failed") {
			attempt.finished, attempt.complete = true, condition.Type == "Complete"
		}
	}
	return attempt, nil
}

func (d *ClusterDriver) pause(ctx context.Context) error {
	poll := d.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(poll):
		return nil
	}
}

// Await waits for THIS run's attempt of a delivery Job — a Job of that name
// carrying the accepted operation's nonce, with a fresh UID — to complete or fail, and
// reads the receipts of that attempt's own pods.
func (d *ClusterDriver) Await(ctx context.Context, job *gitops.DeliveryJob) (Delivery, error) {
	deadline := time.Now().Add(d.AwaitTimeout)
	delivery := Delivery{Job: *job}
	prior := d.priorAttempts[attemptKey(job)]
	notBefore, synced := d.attemptStarts[attemptKey(job)]
	nonce := d.attemptNonces[attemptKey(job)]
	if !synced || nonce == "" {
		return delivery, fmt.Errorf("this run acquired no sync for delivery Job %s, so no attempt of it is this run's", attemptKey(job))
	}
	for {
		attempt, err := d.jobAttempt(ctx, job)
		if err != nil {
			return delivery, err
		}
		if attempt.nonce == nonce && attempt.uid != "" && attempt.uid != prior && !attempt.created.Before(notBefore) && attempt.finished {
			delivery.Succeeded, delivery.Attempt = attempt.complete, attempt.uid
			break
		}
		if time.Now().After(deadline) {
			return delivery, fmt.Errorf("this run's attempt has not finished after %s (the sync this run acquired started %s; the attempt that existed before, %q, is not this run's)", d.AwaitTimeout, notBefore.Format(time.RFC3339), prior)
		}
		if err = d.pause(ctx); err != nil {
			return delivery, err
		}
	}
	log, err := d.Kubectl(ctx, "", "-n", job.Namespace, "logs", "-l", "batch.kubernetes.io/controller-uid="+delivery.Attempt, "--tail=-1")
	if err != nil {
		return delivery, fmt.Errorf("read the log of attempt %s: %w", delivery.Attempt, err)
	}
	delivery.Receipts = gitops.DeliveryReceipts(log)
	return delivery, nil
}
