package localcluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

func localEnvironment() *environments.Environment {
	return &environments.Environment{
		Name:    "local",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d, Context: "k3d-dev"},
		Host: &environments.EnvironmentHost{
			Developers:  []environments.HostDeveloper{{KeyID: "dev-0123456789abcdef"}},
			TrustAnchor: "/etc/host/delivery-trust",
		},
	}
}

type fakeDriver struct {
	steps      []string
	promoted   bool
	fail       string
	deliveries map[string]Delivery
}

func (d *fakeDriver) step(name string) error {
	d.steps = append(d.steps, name)
	if name == d.fail {
		return errors.New(name + " failed")
	}
	return nil
}

func (d *fakeDriver) EnsureArgoCD(context.Context) (bool, error) { return true, d.step("argocd") }
func (d *fakeDriver) EnsureWorkloadIssuer(context.Context) (bool, error) {
	return true, d.step("issuer")
}
func (d *fakeDriver) EnsureDeliveryAccount(context.Context) error {
	return d.step("delivery-account")
}
func (d *fakeDriver) EnsureNamespaces(_ context.Context, modules []string) error {
	return d.step("namespaces " + strings.Join(modules, ","))
}
func (d *fakeDriver) ProvisionTrustAnchor(context.Context) (string, error) {
	return "f00d", d.step("trust-anchor")
}
func (d *fakeDriver) Render(_ context.Context, module string) error {
	return d.step("render " + module)
}
func (d *fakeDriver) Publish(_ context.Context, module string) (gitops.PublishResult, error) {
	return gitops.PublishResult{Path: "modules/" + module, Promoted: d.promoted}, d.step("publish " + module)
}
func (d *fakeDriver) LatestPromotion(context.Context) (string, error) {
	return "abc", d.step("latest")
}
func (d *fakeDriver) Serve(_ context.Context, revision string) error {
	return d.step("serve " + revision)
}
func (d *fakeDriver) Bootstrap(_ context.Context, revision string, published []gitops.PublishResult) ([]gitops.DeliveryJob, error) {
	var jobs []gitops.DeliveryJob
	for _, result := range published {
		jobs = append(jobs, gitops.DeliveryJob{Kind: "presence", Name: "deliver-" + result.Path, Namespace: "ns"})
	}
	return jobs, d.step("bootstrap " + revision)
}
func (d *fakeDriver) Await(_ context.Context, job *gitops.DeliveryJob) (Delivery, error) {
	delivery, known := d.deliveries[job.Name]
	if !known {
		delivery = Delivery{Job: *job, Succeeded: true, Receipts: []string{"delivered " + job.Name + ".json: {}"}}
	}
	return delivery, d.step("await " + job.Name)
}

func quiet(string, ...any) {}

// TestARunTakesEveryModuleThroughTheCellsSteps pins the order: the platform's
// prerequisites, then each module rendered then signed and promoted, then the
// latest promotion served and bootstrapped, then each delivery Job awaited.
func TestARunTakesEveryModuleThroughTheCellsSteps(t *testing.T) {
	driver := &fakeDriver{promoted: true}
	report, err := Run(context.Background(), localEnvironment(), []string{"host", "documents"}, driver, quiet)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"argocd", "issuer", "delivery-account", "namespaces host,documents", "trust-anchor",
		"render host", "publish host", "render documents", "publish documents",
		"latest", "serve abc", "bootstrap abc",
		"await deliver-modules/host", "await deliver-modules/documents",
	}
	if strings.Join(driver.steps, "|") != strings.Join(want, "|") {
		t.Fatalf("steps:\n%v\nwant:\n%v", driver.steps, want)
	}
	if !report.ArgoCDInstalled || !report.WorkloadIssuerInstalled || report.ClusterAuthority != "f00d" || report.Revision != "abc" || len(report.Deliveries) != 2 {
		t.Fatalf("report %+v", report)
	}
}

// TestARunStopsWhereItFailsAndNeverServesAnUnpromotedRevision: a failing step
// stops the run there; a publish that did not promote is refused rather than
// served; a delivery the host refused fails the run with its receipts kept.
func TestARunStopsWhereItFailsAndNeverServesAnUnpromotedRevision(t *testing.T) {
	issuerFailure := &fakeDriver{promoted: true, fail: "issuer"}
	if _, err := Run(context.Background(), localEnvironment(), []string{"host"}, issuerFailure, quiet); err == nil || strings.Join(issuerFailure.steps, ",") != "argocd,issuer" {
		t.Fatalf("issuer failure did not stop before rendering: steps=%v err=%v", issuerFailure.steps, err)
	}
	failing := &fakeDriver{promoted: true, fail: "publish host"}
	if _, err := Run(context.Background(), localEnvironment(), []string{"host", "documents"}, failing, quiet); err == nil {
		t.Fatal("a failed publish did not stop the run")
	}
	if last := failing.steps[len(failing.steps)-1]; last != "publish host" {
		t.Fatalf("the run went on past the failure to %q", last)
	}

	unpromoted := &fakeDriver{}
	if _, err := Run(context.Background(), localEnvironment(), []string{"host"}, unpromoted, quiet); err == nil || !strings.Contains(err.Error(), "did not promote") {
		t.Fatalf("an unpromoted publish was served: %v", err)
	}
	for _, step := range unpromoted.steps {
		if strings.HasPrefix(step, "serve") {
			t.Fatal("an unpromoted revision was served")
		}
	}

	refused := &fakeDriver{promoted: true, deliveries: map[string]Delivery{
		"deliver-modules/host": {Succeeded: false, Receipts: []string{"refused host.json with 401: token review refused"}},
	}}
	report, err := Run(context.Background(), localEnvironment(), []string{"host"}, refused, quiet)
	if err == nil || !strings.Contains(err.Error(), "ns/deliver-modules/host") {
		t.Fatalf("a refused delivery did not fail the run by name: %v", err)
	}
	if len(report.Deliveries) != 1 || report.Deliveries[0].Receipts[0] != "refused host.json with 401: token review refused" {
		t.Fatalf("the refused delivery's receipts were not kept: %+v", report.Deliveries)
	}
}

// TestCheckRefusesWhatALocalClusterRunCannotDeliverTo: a run needs an
// explicitly declared k3d cluster with its context, a host, and the
// developers whose signature delivers to it — checked before anything is
// touched.
func TestCheckRefusesWhatALocalClusterRunCannotDeliverTo(t *testing.T) {
	if err := Check(localEnvironment()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*environments.Environment){
		"no cluster":      func(env *environments.Environment) { env.Cluster = nil },
		"not k3d":         func(env *environments.Environment) { env.Cluster.Kind = "gke" },
		"no context":      func(env *environments.Environment) { env.Cluster.Context = "" },
		"no host":         func(env *environments.Environment) { env.Host = nil },
		"no developers":   func(env *environments.Environment) { env.Host.Developers = nil },
		"no trust anchor": func(env *environments.Environment) { env.Host.TrustAnchor = "" },
	} {
		env := localEnvironment()
		mutate(env)
		if err := Check(env); err == nil {
			t.Errorf("%s: a run was admitted", name)
		}
		driver := &fakeDriver{promoted: true}
		if _, err := Run(context.Background(), env, []string{"host"}, driver, quiet); err == nil || len(driver.steps) != 0 {
			t.Errorf("%s: the run touched the cluster before refusing (%v, %v)", name, err, driver.steps)
		}
	}
}
