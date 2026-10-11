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
}

var _ Driver = (*ClusterDriver)(nil)

func (d *ClusterDriver) EnsureArgoCD(ctx context.Context) (bool, error) {
	return deployments.EnsureArgoCD(ctx, d.Kubectl)
}

func (d *ClusterDriver) EnsureDeliveryAccount(ctx context.Context) error {
	return deployments.EnsureDeliveryAccount(ctx, d.Kubectl)
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
// and ApplicationSet) from the promoted revision and lists the delivery Jobs
// that revision renders for those modules.
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
	for index := range published {
		result := &published[index]
		bootstrap := filepath.Join(tree, filepath.FromSlash(result.Path), "bootstrap")
		if _, statErr := os.Stat(filepath.Join(bootstrap, "kustomization.yaml")); statErr != nil {
			return nil, fmt.Errorf("module tree %s at %s carries no Argo bootstrap; a local cluster run needs the composition's app-project (gitops render --app-project) so Argo CD can sync it", result.Path, revision)
		}
		if _, err = d.Kubectl(ctx, "", "apply", "-k", bootstrap); err != nil {
			return nil, fmt.Errorf("apply the Argo bootstrap of %s: %w", result.Path, err)
		}
		moduleJobs, jobsErr := gitops.DeliveryJobsOf(tree, result.Path, d.Environment.Name)
		if jobsErr != nil {
			return nil, jobsErr
		}
		jobs = append(jobs, moduleJobs...)
	}
	return jobs, nil
}

// Await waits for Argo CD to create the delivery Job (a Sync hook) and for it
// to complete or fail, then reads its receipts from its log.
func (d *ClusterDriver) Await(ctx context.Context, job gitops.DeliveryJob) (Delivery, error) {
	poll := d.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(d.AwaitTimeout)
	delivery := Delivery{Job: job}
	for {
		conditions, err := d.Kubectl(ctx, "", "-n", job.Namespace, "get", "job", job.Name,
			"-o", `jsonpath={range .status.conditions[?(@.status=="True")]}{.type}{" "}{end}`)
		finished := ""
		if err == nil {
			for _, condition := range strings.Fields(conditions) {
				if condition == "Complete" || condition == "Failed" {
					finished = condition
				}
			}
		}
		if finished != "" {
			delivery.Succeeded = finished == "Complete"
			break
		}
		if time.Now().After(deadline) {
			return delivery, fmt.Errorf("not finished after %s (Argo CD creates it when it syncs the module's delivery Application; check the Application and the Job's pod)", d.AwaitTimeout)
		}
		select {
		case <-ctx.Done():
			return delivery, ctx.Err()
		case <-time.After(poll):
		}
	}
	log, err := d.Kubectl(ctx, "", "-n", job.Namespace, "logs", "job/"+job.Name, "--tail=-1")
	if err != nil {
		return delivery, fmt.Errorf("read its log: %w", err)
	}
	delivery.Receipts = gitops.DeliveryReceipts(log)
	return delivery, nil
}
