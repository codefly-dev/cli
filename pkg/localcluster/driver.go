package localcluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/control"
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
	// RolloutTimeout bounds the wait for a trust-anchor rollout to reach
	// every pod (zero: five minutes).
	RolloutTimeout time.Duration

	// priorAttempts holds, per delivery Job, the UID of the attempt that
	// existed before this run delivered ("" for none); attemptStarts, when
	// this run's sync of the Job's Application started, which is the
	// earliest a Job of this run's attempt was created.
	priorAttempts map[string]string
	attemptStarts map[string]time.Time
	attemptNonces map[string]string
	// nonce identifies this run's sync operations (random when empty).
	nonce string
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
// its public-key policy into the verifier's namespace, and
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
	hostModule, _, _ := d.Environment.Host.DeliveryEndpoint()
	namespace := d.Environment.ModuleNamespace(d.Workspace, hostModule)
	if namespace == "" {
		return "", fmt.Errorf("environment %s names no namespace for the host's module %s", d.Environment.Name, hostModule)
	}
	manifest, err := TrustAnchorManifest(namespace, policy)
	if err != nil {
		return "", err
	}
	if _, err = d.Kubectl(ctx, manifest, "apply", "-f", "-"); err != nil {
		return "", fmt.Errorf("write the local trust anchor into %s: %w", namespace, err)
	}
	return fingerprint, d.reconcileTrustAnchor(ctx, namespace, AnchorDigest(policy))
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
	var appliedSets []gitops.ApplicationSetIdentity
	for _, bootstrap := range bootstraps {
		applied, applyErr := d.Kubectl(ctx, "", "apply", "-k", bootstrap, "-o", "json")
		if applyErr != nil {
			return nil, fmt.Errorf("apply the Argo bootstrap at %s: %w", bootstrap, applyErr)
		}
		sets, readErr := appliedApplicationSets(applied)
		if readErr != nil {
			return nil, readErr
		}
		appliedSets = append(appliedSets, sets...)
	}
	if err = bindApplicationOwners(jobs, appliedSets); err != nil {
		return nil, err
	}
	return jobs, d.acquireAttempts(ctx, jobs)
}
