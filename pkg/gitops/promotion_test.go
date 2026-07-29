package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
)

// recordingProducer captures the request it was handed so tests can assert the
// coordinator forwards every field faithfully.
type recordingProducer struct {
	seen ProduceRequest
}

func (r *recordingProducer) Produce(_ context.Context, request ProduceRequest) (RenderResult, error) {
	r.seen = request
	return RenderResult{}, nil
}

func TestCoordinatorForwardsProduceRequestUnchanged(t *testing.T) {
	workspace := &resources.Workspace{}
	module := &resources.Module{Name: "payments"}
	service := &resources.Service{Name: "api"}
	env := &resources.Environment{Name: "local"}

	producer := &recordingProducer{}
	coordinator := &Coordinator{Producer: producer}

	request := ProduceRequest{
		Workspace: workspace, Module: module, Service: service, Environment: env,
		AppProject: "payments", StandAlone: true,
	}
	if _, err := coordinator.Render(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if producer.seen.Service != service || !producer.seen.StandAlone ||
		producer.seen.AppProject != "payments" || producer.seen.Module != module ||
		producer.seen.Environment != env || producer.seen.Workspace != workspace {
		t.Fatalf("coordinator forwarded a mangled request: %+v", producer.seen)
	}
}

// fakeManifestProducer stands in for any conforming plugin: it renders a
// promotable bundle and knows nothing about repositories, reviews, or Argo CD.
type fakeManifestProducer struct {
	module      string
	environment string
	appProject  string
	manifests   string
}

func (f fakeManifestProducer) Produce(ctx context.Context, request ProduceRequest) (RenderResult, error) {
	destination := filepath.Join(request.Workspace.Dir(), "deployments", "modules", f.module)
	return RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination, Module: f.module, Environment: f.environment,
		AppProject: f.appProject, Promotable: true,
	}, func(_ context.Context, stage string) error {
		return os.WriteFile(filepath.Join(stage, "manifests.yaml"), []byte(f.manifests), 0o644)
	})
}

const paymentsAppProject = `---
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: payments
  namespace: argocd
spec:
  sourceRepos:
    - https://github.com/codefly-dev/manifests.git
  destinations:
    - namespace: payments
      server: https://cluster.example.com
`

func TestCoordinatorDrivesFakeProducerThroughPublishAndObserve(t *testing.T) {
	ctx := context.Background()
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	configureSSHSigning(t)

	coordinator := &Coordinator{
		Producer: fakeManifestProducer{
			module: "payments", environment: "local", appProject: "payments",
			manifests: pinnedDeployment + paymentsAppProject,
		},
		Publisher: repositoryPublisher{},
		Observer:  argoObserver{},
	}

	rendered, err := coordinator.Render(ctx, ProduceRequest{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Inventory.Digest == "" {
		t.Fatalf("rendered bundle has no digest: %+v", rendered)
	}

	publish := PublishRequest{
		Module: "payments", Environment: "local", Local: true,
		PromotionBranch: "codefly/promote-payments-local",
	}
	plan, err := coordinator.PlanPublish(ctx, workspace, &publish)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RenderDigest != rendered.Inventory.Digest {
		t.Fatalf("published digest %s does not match produced bundle %s", plan.RenderDigest, rendered.Inventory.Digest)
	}

	result, err := coordinator.Publish(ctx, workspace, &PublishMutation{Request: publish, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Signed || result.Commit == "" {
		t.Fatalf("promotion is not signed and revision-bound: %+v", result)
	}

	installFakeArgo(t, argoProjectJSON(result.Repository), argoApplicationJSON(
		"payments-api", result.Repository, result.Path, result.Commit, "Healthy", "Succeeded",
	))
	observed, err := coordinator.Observe(ctx, &ObserveRequest{
		WorkspaceRoot: workspace.Dir(), Module: "payments", Environment: "local",
		AppProject: "payments", Applications: []string{"payments-api"},
		Repository: result.Repository, Path: result.Path,
		Revision: result.Commit, Commit: result.Commit, Tree: result.Tree,
		RenderDigest: result.RenderDigest, PullRequest: result.PullRequest, Local: true,
		Timeout: time.Second, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Evidence.ArgoRevision != result.Commit || observed.Evidence.Health != "Healthy" {
		t.Fatalf("reconciliation evidence = %+v", observed.Evidence)
	}
	if observed.Evidence.RenderDigest != rendered.Inventory.Digest {
		t.Fatalf("evidence digest %s does not match produced bundle %s", observed.Evidence.RenderDigest, rendered.Inventory.Digest)
	}
}
