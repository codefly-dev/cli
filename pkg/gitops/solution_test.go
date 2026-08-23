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
	overlay := filepath.Join(req.GetDestination(), "overlays", req.GetContext().GetEnvironment())
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		return nil, err
	}
	files := map[string]string{
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - namespace.yaml\n  - configmap.yaml\n",
		"namespace.yaml":     "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: hello\n",
		"configmap.yaml":     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hello\n  namespace: hello\ndata:\n  release: qualified\n",
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

	result, err := RenderSolution(context.Background(), SolutionRenderRequest{
		Workspace:   workspace,
		Environment: env,
		Agent:       agent,
		Name:        "hello",
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
	if filepath.Base(fake.renderDestination) != "hello" || filepath.Base(filepath.Dir(fake.renderDestination)) != solutionUnitDir {
		t.Fatalf("executor render destination %q, want .../solutions/hello", fake.renderDestination)
	}

	inventory := result.Inventory
	if len(inventory.Units) != 1 {
		t.Fatalf("inventory units = %+v", inventory.Units)
	}
	unit := inventory.Units[0]
	if unit.Kind != UnitKindSolution || unit.Name != "hello" || unit.Path != "solutions/hello" {
		t.Fatalf("solution unit = %+v", unit)
	}
	if unit.Output == nil || unit.Output.Validation == nil || !unit.Output.Validation.Promotable {
		t.Fatalf("solution unit output = %+v", unit.Output)
	}

	// The rendered, validated tree is on disk under the solution unit directory.
	loaded, err := LoadInventory(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Module != "hello" || loaded.Namespace != "hello" {
		t.Fatalf("persisted inventory = %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(result.Path, "solutions", "hello", "overlays", "local", "configmap.yaml")); err != nil {
		t.Fatalf("rendered overlay missing: %v", err)
	}
}
