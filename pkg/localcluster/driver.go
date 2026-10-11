package localcluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// ClusterDriver is the Driver a developer's run uses: the gitops render,
// publish and fetch remote, and kubectl against the verified local k3d target.
type ClusterDriver struct {
	Workspace   *resources.Workspace
	Environment *environments.Environment
	Kubectl     deployments.Kubectl
	Plane       control.Plane
	// LoadModule loads a composed module, materializing pinned ones.
	LoadModule func(ctx context.Context, name string) (*resources.Module, error)
	// Approve sees each publish plan before it executes; it may refuse it.
	Approve func(plan *gitops.PublishPlan) error
	// Sink, Rebuild and AppProject shape each render: the AppProject is what
	// the module's Argo bootstrap is stamped with.
	Sink       orchestration.OutputSink
	Rebuild    bool
	AppProject string
	// AwaitTimeout bounds the wait for each delivery Job.
	AwaitTimeout time.Duration
	// Poll is the interval between Job status reads (zero: two seconds).
	Poll time.Duration
	// TrustedRoot fetches the trusted root the local host's anchor holds
	// (nil: the public-good root, through TUF).
	TrustedRoot func(ctx context.Context) ([]byte, error)

	// priorAttempts holds, per delivery Job, the UID of the attempt that
	// existed before this run delivered ("" for none).
	priorAttempts map[string]string
}

var _ Driver = (*ClusterDriver)(nil)

func (d *ClusterDriver) EnsureArgoCD(ctx context.Context) (bool, error) {
	return deployments.EnsureArgoCD(ctx, d.Kubectl, nil)
}

func (d *ClusterDriver) EnsureDeliveryAccount(ctx context.Context) error {
	return deployments.EnsureDeliveryAccount(ctx, d.Kubectl)
}

func (d *ClusterDriver) EnsureNamespaces(ctx context.Context, modules []string) error {
	var manifest strings.Builder
	seen := map[string]bool{}
	for _, module := range modules {
		namespace := d.Environment.ModuleNamespace(d.Workspace, module)
		if namespace == "" || seen[namespace] {
			continue
		}
		seen[namespace] = true
		manifest.WriteString("---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + namespace + "\n  labels:\n    app.kubernetes.io/managed-by: codefly-local-cluster-run\n")
	}
	if manifest.Len() == 0 {
		return fmt.Errorf("environment %s names no namespace for the modules", d.Environment.Name)
	}
	if _, err := d.Kubectl(ctx, manifest.String(), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("provision the modules' namespaces: %w", err)
	}
	return nil
}

// ProvisionTrustAnchor binds the local profile to this cluster's CA, writes
// it with the public-good trusted root into the verifier's namespace, and
// reconciles every workload that mounts it to the anchor's digest
// (reconcileTrustAnchor): the host reads its anchor at boot, so a changed
// anchor — a recreated cluster, a developer added or REMOVED — is in force
// only once the pods running the verifier started after it.
func (d *ClusterDriver) ProvisionTrustAnchor(ctx context.Context) (string, error) {
	bundle, err := d.Kubectl(ctx, "", "-n", "default", "get", "configmap", "kube-root-ca.crt", "-o", `jsonpath={.data.ca\.crt}`)
	if err != nil {
		return "", fmt.Errorf("read the cluster CA (kube-root-ca.crt): %w", err)
	}
	fingerprint, err := ClusterCAFingerprint([]byte(bundle))
	if err != nil {
		return "", err
	}
	policy, err := VerificationPolicy(fingerprint, d.Environment.Host)
	if err != nil {
		return "", err
	}
	fetch := d.TrustedRoot
	if fetch == nil {
		fetch = signing.PublicGoodTrustedRootJSON
	}
	trustedRoot, err := fetch(ctx)
	if err != nil {
		return "", err
	}
	hostModule, _, _ := d.Environment.Host.DeliveryEndpoint()
	namespace := d.Environment.ModuleNamespace(d.Workspace, hostModule)
	if namespace == "" {
		return "", fmt.Errorf("environment %s names no namespace for the host's module %s", d.Environment.Name, hostModule)
	}
	manifest, err := TrustAnchorManifest(namespace, policy, trustedRoot)
	if err != nil {
		return "", err
	}
	if _, err = d.Kubectl(ctx, manifest, "apply", "-f", "-"); err != nil {
		return "", fmt.Errorf("write the local trust anchor into %s: %w", namespace, err)
	}
	return fingerprint, d.reconcileTrustAnchor(ctx, namespace, AnchorDigest(policy, trustedRoot))
}

// TrustAnchorAnnotation is the pod-template annotation that records which
// trust anchor a verifier's pods were started with.
const TrustAnchorAnnotation = "codefly.dev/trust-anchor"

// AnchorDigest is the digest of both trust-anchor objects' contents, by file.
func AnchorDigest(policy, trustedRoot []byte) string {
	hash := sha256.New()
	for _, part := range [][]byte{[]byte(VerificationPolicyFile), policy, []byte(TrustedRootFile), trustedRoot} {
		_, _ = fmt.Fprintf(hash, "%d:", len(part))
		hash.Write(part)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// anchoredWorkloads is the part of a workload list the reconciliation reads.
type anchoredWorkloads struct {
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
				Spec struct {
					Volumes []struct {
						Name string `json:"name"`
					} `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	} `json:"items"`
}

// reconcileTrustAnchor stamps the anchor's digest on the pod template of every
// Deployment, StatefulSet and DaemonSet in the namespace that mounts the local
// trust anchor, and waits for each rollout to complete. It is decided by the
// workloads' own state, never by what the last apply reported: a stamp or a
// rollout that failed leaves a workload behind the digest, and the next run
// finds it there and stamps or waits again, until the running pods were
// started with the anchor in force.
func (d *ClusterDriver) reconcileTrustAnchor(ctx context.Context, namespace, digest string) error {
	listed, err := d.Kubectl(ctx, "", "-n", namespace, "get", "deployments,statefulsets,daemonsets", "-o", "json")
	if err != nil {
		return fmt.Errorf("list the workloads mounting the local trust anchor in %s: %w", namespace, err)
	}
	var workloads anchoredWorkloads
	if err = json.Unmarshal([]byte(listed), &workloads); err != nil {
		return fmt.Errorf("read the workloads of %s: %w", namespace, err)
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{keyMetadata: map[string]any{
		"annotations": map[string]string{TrustAnchorAnnotation: digest},
	}}}})
	if err != nil {
		return err
	}
	for index := range workloads.Items {
		workload := &workloads.Items[index]
		mounts := false
		for _, volume := range workload.Spec.Template.Spec.Volumes {
			mounts = mounts || volume.Name == gitops.LocalTrustAnchorVolume
		}
		if !mounts {
			continue
		}
		reference := strings.ToLower(workload.Kind) + "/" + workload.Metadata.Name
		if workload.Spec.Template.Metadata.Annotations[TrustAnchorAnnotation] != digest {
			if _, err = d.Kubectl(ctx, "", "-n", namespace, "patch", reference, "--type", "merge", "-p", string(patch)); err != nil {
				return fmt.Errorf("stamp the trust anchor %s on %s (the next run retries): %w", digest, reference, err)
			}
		}
		if _, err = d.Kubectl(ctx, "", "-n", namespace, "rollout", "status", reference, "--timeout=5m"); err != nil {
			return fmt.Errorf("%s has not rolled out on the trust anchor %s (the next run waits again): %w", reference, digest, err)
		}
	}
	return nil
}

func (d *ClusterDriver) Render(ctx context.Context, name string) error {
	module, err := d.LoadModule(ctx, name)
	if err != nil {
		return err
	}
	_, err = gitops.NewCoordinator().Render(ctx, gitops.ProduceRequest{
		Workspace: d.Workspace, Module: module, Environment: d.Environment,
		AppProject: d.AppProject, Rebuild: d.Rebuild, Sink: d.Sink,
	})
	return err
}

func (d *ClusterDriver) Publish(ctx context.Context, module string) (gitops.PublishResult, error) {
	request := gitops.PublishRequest{Module: module, Environment: d.Environment.Name, Local: true}
	return control.PublishGitOps(ctx, d.Plane, &request, d.Approve)
}

func (d *ClusterDriver) LatestPromotion(ctx context.Context) (string, error) {
	return gitops.LatestLocalPromotion(ctx, d.Workspace, d.Environment.Name)
}

// Serve brings the fetch remote up on the revision and has the cluster's
// Argo CD trust the remote's CA for its name (argocd-tls-certs-cm), which is
// how Argo CD trusts a repository server's certificate.
func (d *ClusterDriver) Serve(ctx context.Context, revision string) error {
	remote, err := gitops.NewFetchRemote(d.Workspace, d.Environment.Name)
	if err != nil {
		return err
	}
	status, err := remote.Up(ctx, revision)
	if err != nil {
		return err
	}
	if status.CACertPath == "" {
		return fmt.Errorf("the fetch remote reports no CA for Argo CD to trust")
	}
	ca, err := os.ReadFile(status.CACertPath)
	if err != nil {
		return fmt.Errorf("read the fetch remote's CA: %w", err)
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{remote.Spec.DNSName: string(ca)}})
	if err != nil {
		return err
	}
	if _, err = d.Kubectl(ctx, "", "-n", "argocd", "patch", "configmap", "argocd-tls-certs-cm", "--type", "merge", "-p", string(patch)); err != nil {
		return fmt.Errorf("make Argo CD trust the fetch remote %s: %w", remote.Spec.DNSName, err)
	}
	return nil
}

// Bootstrap applies each published module's Argo bootstrap (its AppProjects
// and ApplicationSet) from the promoted revision, lists the delivery Jobs
// that revision renders for those modules, and makes sure each gets an
// attempt of THIS run: the attempt that existed before is recorded first, and
// an Application whose Job already ran is asked for a fresh sync.
func (d *ClusterDriver) Bootstrap(ctx context.Context, revision string, published []gitops.PublishResult) ([]gitops.DeliveryJob, error) {
	parent, err := os.MkdirTemp("", "codefly-local-promotion-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(parent)
	tree := filepath.Join(parent, "tree")
	if err = gitops.ExportPromotion(ctx, d.Workspace, d.Environment.Name, revision, tree); err != nil {
		return nil, err
	}
	var jobs []gitops.DeliveryJob
	var bootstraps []string
	for index := range published {
		result := &published[index]
		bootstrap := filepath.Join(tree, filepath.FromSlash(result.Path), "bootstrap")
		if _, statErr := os.Stat(filepath.Join(bootstrap, "kustomization.yaml")); statErr != nil {
			return nil, fmt.Errorf("module tree %s at %s carries no Argo bootstrap; a local cluster run needs the composition's app-project (gitops render --app-project) so Argo CD can sync it", result.Path, revision)
		}
		bootstraps = append(bootstraps, bootstrap)
		moduleJobs, jobsErr := gitops.DeliveryJobsOf(tree, result.Path, d.Environment.Name)
		if jobsErr != nil {
			return nil, jobsErr
		}
		jobs = append(jobs, moduleJobs...)
	}
	// Which attempt of each Job is THIS run's is settled before anything
	// can create one: the Job present now (if any) is a previous attempt.
	if err = d.recordPriorAttempts(ctx, jobs); err != nil {
		return nil, err
	}
	for _, bootstrap := range bootstraps {
		if _, err = d.Kubectl(ctx, "", "apply", "-k", bootstrap); err != nil {
			return nil, fmt.Errorf("apply the Argo bootstrap at %s: %w", bootstrap, err)
		}
	}
	return jobs, d.requestAttempts(ctx, jobs)
}

// attemptKey names a delivery Job by namespace and name.
func attemptKey(job *gitops.DeliveryJob) string { return job.Namespace + "/" + job.Name }

// recordPriorAttempts reads the UID of each delivery Job that exists before
// this run delivers anything. A Job's name is the digest of the carriers it
// posts, so an unchanged set reuses the name of a Job an EARLIER run created
// — and finished: its outcome says nothing about this run's delivery.
func (d *ClusterDriver) recordPriorAttempts(ctx context.Context, jobs []gitops.DeliveryJob) error {
	d.priorAttempts = map[string]string{}
	for index := range jobs {
		job := &jobs[index]
		attempt, err := d.jobAttempt(ctx, job)
		if err != nil {
			return err
		}
		d.priorAttempts[attemptKey(job)] = attempt.uid
	}
	return nil
}

// requestAttempts asks Argo CD for a fresh sync of every delivery Application
// whose Job a previous run already ran: a Sync hook runs again on each sync
// (BeforeHookCreation replaces the Job of the same name), so this run's
// delivery is a new Job attempt, with a new UID. A Job that did not exist yet
// is created by the sync the bootstrap starts.
func (d *ClusterDriver) requestAttempts(ctx context.Context, jobs []gitops.DeliveryJob) error {
	for index := range jobs {
		job := &jobs[index]
		if d.priorAttempts[attemptKey(job)] == "" {
			continue
		}
		application, err := d.deliveryApplication(ctx, job)
		if err != nil {
			return err
		}
		operation, err := json.Marshal(map[string]any{"operation": map[string]any{
			"initiatedBy": map[string]any{"username": "codefly-run-cluster"},
			"info":        []any{map[string]string{"name": "redeliver", "value": attemptKey(job)}},
			"sync":        map[string]any{},
		}})
		if err != nil {
			return err
		}
		if _, err = d.Kubectl(ctx, "", "-n", "argocd", "patch", "application", application, "--type", "merge", "-p", string(operation)); err != nil {
			return fmt.Errorf("request a fresh delivery from Application %s: %w", application, err)
		}
	}
	return nil
}

// deliveryApplication finds the Argo CD Application that syncs the overlay a
// delivery Job is rendered in, waiting for the ApplicationSet to create it.
func (d *ClusterDriver) deliveryApplication(ctx context.Context, job *gitops.DeliveryJob) (string, error) {
	overlay := path.Dir(job.Path)
	deadline := time.Now().Add(d.AwaitTimeout)
	for {
		listed, err := d.Kubectl(ctx, "", "-n", "argocd", "get", "applications", "-o", "json")
		if err == nil {
			var applications struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Spec struct {
						Source struct {
							Path string `json:"path"`
						} `json:"source"`
					} `json:"spec"`
				} `json:"items"`
			}
			if json.Unmarshal([]byte(listed), &applications) == nil {
				for _, application := range applications.Items {
					if path.Clean(application.Spec.Source.Path) == overlay {
						return application.Metadata.Name, nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no Argo CD Application syncs %s after %s; check the module's ApplicationSet", overlay, d.AwaitTimeout)
		}
		if err = d.pause(ctx); err != nil {
			return "", err
		}
	}
}

// jobAttempt is one attempt of a delivery Job: its UID and, once finished,
// whether it completed.
type jobAttempt struct {
	uid      string
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
				UID string `json:"uid"`
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
	attempt := jobAttempt{uid: jobs.Items[0].Metadata.UID}
	for _, condition := range jobs.Items[0].Status.Conditions {
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
// whose UID is not the one that existed before the run delivered — to
// complete or fail, and reads the receipts of that attempt's own pods. A
// previous run's Job of the same name, finished or not, is never its answer.
func (d *ClusterDriver) Await(ctx context.Context, job gitops.DeliveryJob) (Delivery, error) {
	deadline := time.Now().Add(d.AwaitTimeout)
	delivery := Delivery{Job: job}
	prior := d.priorAttempts[attemptKey(&job)]
	for {
		attempt, err := d.jobAttempt(ctx, &job)
		if err != nil {
			return delivery, err
		}
		if attempt.uid != "" && attempt.uid != prior && attempt.finished {
			delivery.Succeeded, delivery.Attempt = attempt.complete, attempt.uid
			break
		}
		if time.Now().After(deadline) {
			return delivery, fmt.Errorf("this run's attempt has not finished after %s (the attempt that existed before the run, %q, is not this run's; check the delivery Application's sync and the Job's pod)", d.AwaitTimeout, prior)
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
