// Package localcluster is the local half of solution and module delivery
// (cli#918): one run that takes a composition onto the developer's k3d
// cluster through the SAME delivery a cell receives. Each module is built and
// rendered (presence and authority derived from its contract, against the real
// bytes the build produced), published as a promotion the developer signs —
// on a local cluster the developer's signed publish is the approval, so it
// advances the local base branch itself — and served to the cluster's Argo
// CD, which syncs the tree and runs the delivery Jobs that POST the signed
// carriers to the local host. The run reports each Job's receipts.
//
// There is no local trust mode and no second path: every step is a step a
// cell runs, and the only local data is the k3d environment's host block
// (its developers in place of a release workflow).
package localcluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

// Driver is every side effect of a run, in the order the run takes them.
type Driver interface {
	// EnsureArgoCD installs Argo CD unless the cluster serves it already.
	EnsureArgoCD(ctx context.Context) (installed bool, err error)
	// EnsureDeliveryAccount provisions the authority namespace and its
	// delivery account, which a cell's platform provides.
	EnsureDeliveryAccount(ctx context.Context) error
	// EnsureNamespaces provisions the modules' namespaces, which a cell's
	// platform provides (every Application syncs with CreateNamespace=false).
	EnsureNamespaces(ctx context.Context, modules []string) error
	// ProvisionTrustAnchor writes the local host's trust anchor for this
	// cluster and returns the cluster CA fingerprint it is bound to.
	ProvisionTrustAnchor(ctx context.Context) (string, error)
	// Render builds the module's images and renders its tree.
	Render(ctx context.Context, module string) error
	// Publish plans, signs as the developer, and promotes the module.
	Publish(ctx context.Context, module string) (gitops.PublishResult, error)
	// LatestPromotion is the environment's promoted revision.
	LatestPromotion(ctx context.Context) (string, error)
	// Serve brings the fetch remote up on the revision and makes the
	// cluster's Argo CD trust it.
	Serve(ctx context.Context, revision string) error
	// ApplySecrets writes each published module's local Secrets — the ones a
	// cell's store would hold — to the cluster before anything reads them.
	ApplySecrets(ctx context.Context, published []gitops.PublishResult) error
	// Bootstrap applies the published modules' Argo bootstrap at the
	// revision and returns the delivery Jobs that revision renders.
	Bootstrap(ctx context.Context, revision string, published []gitops.PublishResult) ([]gitops.DeliveryJob, error)
	// Await waits for a delivery Job to finish and returns its receipts.
	Await(ctx context.Context, job *gitops.DeliveryJob) (Delivery, error)
}

// Delivery is what one delivery Job answered.
type Delivery struct {
	Job gitops.DeliveryJob `json:"job"`
	// Attempt is the UID of the Job attempt this run read: never one that
	// existed before the run delivered.
	Attempt   string   `json:"attempt"`
	Succeeded bool     `json:"succeeded"`
	Receipts  []string `json:"receipts"`
}

// Report is what a run did.
type Report struct {
	ArgoCDInstalled  bool                   `json:"argoCDInstalled"`
	ClusterAuthority string                 `json:"clusterAuthority"`
	Published        []gitops.PublishResult `json:"published"`
	Revision         string                 `json:"revision"`
	Deliveries       []Delivery             `json:"deliveries"`
}

// Check holds an environment to what a local cluster run needs before
// anything is touched: an explicitly declared k3d cluster with its context
// (the legacy "an undeclared local environment is k3d" reading never selects
// a cluster to install into), and a host block listing the developers whose
// signed publish delivers to it.
func Check(env *environments.Environment) error {
	if env == nil {
		return fmt.Errorf("an environment is required")
	}
	if !env.DeclaresLocalK3d() {
		return fmt.Errorf("environment %s does not declare a k3d cluster with its context (cluster.kind: k3d, cluster.context: k3d-<name>); a local cluster run takes a composition onto a developer's k3d cluster only", env.Name)
	}
	if env.Host == nil {
		return fmt.Errorf("environment %s declares no host, so a cluster run would deliver nothing to it; declare the local host (see docs/solution-host-bindings.md, \"Local runs\")", env.Name)
	}
	if len(env.Host.Developers) == 0 {
		return fmt.Errorf("environment %s lists no developer in host.developers; a local run signs as the developer the local host admits, and an unsigned delivery is refused by every host", env.Name)
	}
	if env.Host.TrustAnchor == "" {
		return fmt.Errorf("environment %s states no host.trust_anchor; a local run mounts the local host's trust anchor where the host reads it, which is the host's own constant path", env.Name)
	}
	return nil
}

// Run takes the modules onto the cluster: platform prerequisites, then each
// module rendered and promoted in order, then the latest promotion served and
// bootstrapped, then every delivery Job awaited. It stops at the first step
// that fails, after reporting what it did; a refused delivery is reported,
// not hidden, and fails the run.
func Run(ctx context.Context, env *environments.Environment, modules []string, driver Driver, say func(format string, args ...any)) (Report, error) {
	var report Report
	if err := Check(env); err != nil {
		return report, err
	}
	if len(modules) == 0 {
		return report, fmt.Errorf("no module to run")
	}
	installed, err := driver.EnsureArgoCD(ctx)
	if err != nil {
		return report, err
	}
	report.ArgoCDInstalled = installed
	if installed {
		say("Installed Argo CD (%s) into the cluster", "argocd")
	}
	if err = driver.EnsureDeliveryAccount(ctx); err != nil {
		return report, err
	}
	if err = driver.EnsureNamespaces(ctx, modules); err != nil {
		return report, err
	}
	// Before any module syncs: the host reads its anchor at boot, so it
	// must exist when the host's pod first starts.
	report.ClusterAuthority, err = driver.ProvisionTrustAnchor(ctx)
	if err != nil {
		return report, fmt.Errorf("provision the local host's trust anchor: %w", err)
	}
	say("Local host trust anchor bound to the cluster CA %s, admitting %d developer(s)", report.ClusterAuthority, len(env.Host.Developers))
	for _, module := range modules {
		say("Rendering %s for %s", module, env.Name)
		if err = driver.Render(ctx, module); err != nil {
			return report, fmt.Errorf("render %s: %w", module, err)
		}
		say("Publishing %s: signing as a developer %s lists, then promoting locally", module, env.Name)
		result, publishErr := driver.Publish(ctx, module)
		if publishErr != nil {
			return report, fmt.Errorf("publish %s: %w", module, publishErr)
		}
		if !result.Promoted {
			return report, fmt.Errorf("publish %s did not promote locally; a cluster run serves only promoted revisions", module)
		}
		report.Published = append(report.Published, result)
		for _, document := range deliveredDocuments(&result) {
			say("  %s", document)
		}
	}
	report.Revision, err = driver.LatestPromotion(ctx)
	if err != nil {
		return report, err
	}
	say("Serving the promoted revision %s to the cluster", report.Revision)
	if err = driver.Serve(ctx, report.Revision); err != nil {
		return report, err
	}
	if err = driver.ApplySecrets(ctx, report.Published); err != nil {
		return report, err
	}
	jobs, err := driver.Bootstrap(ctx, report.Revision, report.Published)
	if err != nil {
		return report, err
	}
	var failed []string
	for index := range jobs {
		job := &jobs[index]
		say("Awaiting delivery Job %s/%s (%s)", job.Namespace, job.Name, job.Kind)
		delivery, awaitErr := driver.Await(ctx, job)
		if awaitErr != nil {
			return report, fmt.Errorf("delivery Job %s/%s: %w", job.Namespace, job.Name, awaitErr)
		}
		report.Deliveries = append(report.Deliveries, delivery)
		for _, receipt := range delivery.Receipts {
			say("  %s", receipt)
		}
		if !delivery.Succeeded {
			failed = append(failed, job.Namespace+"/"+job.Name)
		}
	}
	if len(failed) > 0 {
		return report, fmt.Errorf("the host refused or never answered delivery Job(s) %s; their receipts are above", strings.Join(failed, ", "))
	}
	return report, nil
}

// deliveredDocuments describes what a publish settled, one line per document.
func deliveredDocuments(result *gitops.PublishResult) []string {
	if result.Delivery == nil {
		return nil
	}
	lines := make([]string, 0, len(result.Delivery.Documents))
	for _, document := range result.Delivery.Documents {
		state := "settled"
		if document.Removed {
			state = "withdrawn by a tombstone"
		}
		lines = append(lines, fmt.Sprintf("%s %s at generation %d, %s, signed by %s", document.Kind, document.ID, document.Generation, state, result.Delivery.Identity))
	}
	return lines
}
