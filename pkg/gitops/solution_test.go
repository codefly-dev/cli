package gitops

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	solutionv0 "github.com/codefly-dev/core/generated/go/codefly/services/solution/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"strings"
)

// fakeSolutionExecutor is an in-process codefly:solution executor: Package
// returns a pushed OCI reference, Render writes a promotable overlay tree into
// the requested destination. It records what the host asked it to do so the test
// can assert the render pipeline drives the executor contract correctly.
type fakeSolutionExecutor struct {
	solutionv0.UnimplementedSolutionServer
	packageSource     string
	renderArtifact    string
	renderDestination string
	renderNamespace   string
	environment       string
}

func (f *fakeSolutionExecutor) Package(_ context.Context, req *solutionv0.PackageRequest) (*solutionv0.PackageResponse, error) {
	f.packageSource = req.GetSource()
	f.environment = req.GetContext().GetEnvironment()
	return &solutionv0.PackageResponse{
		Reference:      req.GetReference() + "@sha256:" + digestPlaceholder,
		ArtifactDigest: "sha256:" + digestPlaceholder,
	}, nil
}

func (f *fakeSolutionExecutor) Render(_ context.Context, req *solutionv0.RenderRequest) (*solutionv0.RenderResponse, error) {
	f.renderArtifact = req.GetArtifactReference()
	f.renderDestination = req.GetDestination()
	// A real executor cannot know the target namespace; the host supplies it.
	f.renderNamespace = req.GetValues()[SolutionNamespaceValue]
	if f.renderNamespace == "" {
		return nil, fmt.Errorf("render was not told the target namespace")
	}
	overlay := filepath.Join(req.GetDestination(), "overlays", req.GetContext().GetEnvironment())
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		return nil, err
	}
	files := map[string]string{
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - namespace.yaml\n  - configmap.yaml\n",
		"namespace.yaml":     fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", f.renderNamespace),
		"configmap.yaml":     fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hello\n  namespace: %s\ndata:\n  release: qualified\n", f.renderNamespace),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(overlay, name), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	return &solutionv0.RenderResponse{RenderedPaths: []string{"overlays/" + req.GetContext().GetEnvironment()}}, nil
}

const digestPlaceholder = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func serveFakeSolutionExecutor(t *testing.T, server solutionv0.SolutionServer) solutionExecutor {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	solutionv0.RegisterSolutionServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(solution.EnforcingClientInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return solution.NewClient(conn)
}

// installFakeSolutionExecutor serves an in-process solution executor and routes
// RenderSolution to it for the duration of the test, standing in for the
// verified-artifact plugin so the render pipeline is exercised end to end.
func installFakeSolutionExecutor(t *testing.T, server solutionv0.SolutionServer) {
	t.Helper()
	client := serveFakeSolutionExecutor(t, server)
	previous := connectSolutionExecutor
	connectSolutionExecutor = func(context.Context, string, *resources.Agent) (solutionExecutor, func(), error) {
		return client, func() {}, nil
	}
	t.Cleanup(func() { connectSolutionExecutor = previous })
}

func loadSolutionWorkspace(t *testing.T, remote string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	config := fmt.Sprintf(`name: hello
layout: flat
environments:
  - name: local
    namespace: hello
    cluster:
      kind: k3d
gitops:
  repo-url: file://%s
  fetch-repo-url: https://host.k3d.internal/manifests.git
  path: environments
  branch: main
`, remote)
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestRenderSolutionDrivesExecutorToPromotableOwnedTree(t *testing.T) {
	fake := &fakeSolutionExecutor{}
	installFakeSolutionExecutor(t, fake)

	workspace := loadSolutionWorkspace(t, "/tmp/hello.git")
	env := workspace.FindEnvironment("local")
	if env == nil {
		t.Fatal("environment local not found")
	}
	agent := &resources.Agent{
		Kind: resources.SolutionAgent, Publisher: "codefly.dev", Name: "hello-solution", Version: "0.0.1",
	}

	// The solution id differs from the environment namespace ("hello") so the
	// assertions below prove the render targets the solution's own namespace, not
	// the shared host namespace.
	result, err := RenderSolution(context.Background(), &SolutionRenderRequest{
		Workspace:   workspace,
		Environment: env,
		Agent:       agent,
		Name:        "lastlogin-go",
		Source:      filepath.Join(workspace.Dir(), "solution-src"),
		Reference:   "ghcr.io/codefly-dev/hello-solution:0.0.1",
		AppProject:  "hello",
	})
	if err != nil {
		t.Fatalf("RenderSolution: %v", err)
	}

	if fake.packageSource != filepath.Join(workspace.Dir(), "solution-src") {
		t.Fatalf("executor packaged source %q", fake.packageSource)
	}
	if fake.environment != "local" {
		t.Fatalf("executor context environment %q", fake.environment)
	}
	// Render must consume the reference Package pushed, not the target reference.
	if fake.renderArtifact != "ghcr.io/codefly-dev/hello-solution:0.0.1@sha256:"+digestPlaceholder {
		t.Fatalf("executor rendered from artifact %q", fake.renderArtifact)
	}
	if filepath.Base(fake.renderDestination) != "lastlogin-go" || filepath.Base(filepath.Dir(fake.renderDestination)) != solutionUnitDir {
		t.Fatalf("executor render destination %q, want .../solutions/lastlogin-go", fake.renderDestination)
	}
	// The host must tell the executor the solution's own namespace, derived from
	// its id — not the shared environment namespace "hello".
	if fake.renderNamespace != "lastlogin-go" {
		t.Fatalf("executor was told namespace %q, want the solution namespace %q", fake.renderNamespace, "lastlogin-go")
	}

	inventory := result.Inventory
	if len(inventory.Units) != 1 {
		t.Fatalf("inventory units = %+v", inventory.Units)
	}
	unit := inventory.Units[0]
	if unit.Kind != UnitKindSolution || unit.Name != "lastlogin-go" || unit.Path != "solutions/lastlogin-go" {
		t.Fatalf("solution unit = %+v", unit)
	}
	if unit.Output == nil || unit.Output.Validation == nil || !unit.Output.Validation.Promotable {
		t.Fatalf("solution unit output = %+v", unit.Output)
	}

	// The rendered, validated tree is on disk under the solution unit directory,
	// and its inventory namespace is the solution's own, not the environment's.
	loaded, err := LoadInventory(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Module != "lastlogin-go" || loaded.Namespace != "lastlogin-go" {
		t.Fatalf("persisted inventory = %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(result.Path, "solutions", "lastlogin-go", "overlays", "local", "configmap.yaml")); err != nil {
		t.Fatalf("rendered overlay missing: %v", err)
	}
}

func TestSolutionPublicationDoesNotRequireAModuleResource(t *testing.T) {
	// A workspace named "hello" with no module "checkout": if the publish path
	// tried to load a service module for a solution, this would error.
	workspace := loadSolutionWorkspace(t, "/tmp/hello.git")
	inventory := &Inventory{Units: []InventoryUnit{
		{Kind: UnitKindSolution, Module: "checkout", Name: "checkout", Path: "solutions/checkout"},
	}}
	generate, err := publicationGeneratesBootstrap(context.Background(), workspace, "checkout", inventory)
	if err != nil || !generate {
		t.Fatalf("publicationGeneratesBootstrap = (%v, %v), want (true, nil) with no module load", generate, err)
	}
	if err := validateSolutionUnits("checkout", inventory.Units); err != nil {
		t.Fatalf("validateSolutionUnits: %v", err)
	}
	// A service unit smuggled into a solution publication must be rejected.
	mixed := []InventoryUnit{{Kind: UnitKindService, Module: "checkout", Name: "api", Path: "services/api"}}
	if err := validateSolutionUnits("checkout", mixed); err == nil {
		t.Fatal("validateSolutionUnits accepted a non-solution unit")
	}
}

// TestLocalGitopsPublishSolutionGeneratesBootstrap drives the real publish path
// for a solution and asserts the CLI generates the Argo ApplicationSet — the
// transport a solution has no service module or module-agent to trigger. Without
// the publish fix this produces a tree with no bootstrap, so the solution never
// reaches ArgoCD; the assertion here is what catches that regression.
func TestLocalGitopsPublishSolutionGeneratesBootstrap(t *testing.T) {
	ctx := context.Background()
	installFakeSolutionExecutor(t, &fakeSolutionExecutor{})
	remote := createBareRepository(t)
	workspace := loadSolutionWorkspace(t, remote)
	env := workspace.FindEnvironment("local")
	if env == nil {
		t.Fatal("environment local not found")
	}
	agent := &resources.Agent{
		Kind: resources.SolutionAgent, Publisher: "codefly.dev", Name: "hello-solution", Version: "0.0.1",
	}
	if _, err := RenderSolution(ctx, &SolutionRenderRequest{
		Workspace: workspace, Environment: env, Agent: agent, Name: "lastlogin-go",
		Source:     filepath.Join(workspace.Dir(), "solution-src"),
		Reference:  "ghcr.io/codefly-dev/hello-solution:0.0.1",
		AppProject: "lastlogin-go",
	}); err != nil {
		t.Fatal(err)
	}
	configureSSHSigning(t)

	request := PublishRequest{
		Module: "lastlogin-go", Environment: "local", Local: true,
		PromotionBranch: "codefly/promote-lastlogin-go-local",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}

	appSet := gitOutput(t, "", "--git-dir", remote, "show", result.Commit+":"+result.Path+"/bootstrap/applicationset.yaml")
	if !strings.Contains(appSet, "kind: ApplicationSet") ||
		!strings.Contains(appSet, "overlay: "+result.Path+"/solutions/lastlogin-go/overlays/local") {
		t.Fatalf("published bootstrap does not stamp the solution Application:\n%s", appSet)
	}
	// The packaged solution's own Namespace is authorized by the generated
	// AppProject: destination namespace plus a cluster-scoped Namespace whitelist.
	// It is the solution's own namespace ("lastlogin-go"), not the shared host
	// namespace ("hello").
	project := gitOutput(t, "", "--git-dir", remote, "show", result.Commit+":"+result.Path+"/bootstrap/project.yaml")
	if !strings.Contains(project, "namespace: lastlogin-go") || !strings.Contains(project, "kind: Namespace") {
		t.Fatalf("generated AppProject does not authorize the solution namespace:\n%s", project)
	}
	if strings.Contains(project, "namespace: hello") {
		t.Fatalf("generated AppProject leaks the shared host namespace:\n%s", project)
	}
}
