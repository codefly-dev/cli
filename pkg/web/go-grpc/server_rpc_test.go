package go_grpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/codefly-dev/core/architecture"
	observabilityv0 "github.com/codefly-dev/core/generated/go/codefly/observability/v0"
	"github.com/codefly-dev/core/resources"
)

func writeGraphWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                              "name: demo\nlayout: modules\nmodules:\n    - name: backend\n",
		"modules/backend/module.codefly.yaml":                 "kind: module\nname: backend\nservices:\n    - name: api\n    - name: store\n",
		"modules/backend/services/api/service.codefly.yaml":   "kind: service\nname: api\nversion: 0.0.0\nmodule: backend\nagent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\nservice-dependencies:\n    - name: store\n      module: backend\n",
		"modules/backend/services/store/service.codefly.yaml": "kind: service\nname: store\nversion: 0.0.0\nmodule: backend\nagent:\n    kind: runtime::service\n    name: postgres\n    version: 0.0.10\n    publisher: codefly.ai\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func loadGraphWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), writeGraphWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func nodeType(resp *observabilityv0.GraphResponse, id string) (observabilityv0.GraphNode_Type, bool) {
	for _, n := range resp.Nodes {
		if n.Id == id {
			return n.Type, true
		}
	}
	return 0, false
}

func hasEdge(resp *observabilityv0.GraphResponse, from, to string) bool {
	for _, e := range resp.Edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func TestServiceDependencyGraphFromWorkspace(t *testing.T) {
	ws := loadGraphWorkspace(t)
	ctx := context.Background()

	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0", EndpointRest: "127.0.0.1:0"}, ws, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := server.GetWorkspaceServiceDependencyGraph(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}

	if typ, ok := nodeType(resp, "backend"); !ok || typ != observabilityv0.GraphNode_MODULE {
		t.Fatalf("expected MODULE node %q, got type=%v ok=%v", "backend", typ, ok)
	}
	if typ, ok := nodeType(resp, "backend/api"); !ok || typ != observabilityv0.GraphNode_SERVICE {
		t.Fatalf("expected SERVICE node %q, got type=%v ok=%v", "backend/api", typ, ok)
	}
	if typ, ok := nodeType(resp, "backend/store"); !ok || typ != observabilityv0.GraphNode_SERVICE {
		t.Fatalf("expected SERVICE node %q, got type=%v ok=%v", "backend/store", typ, ok)
	}

	if !hasEdge(resp, "backend", "backend/api") {
		t.Fatalf("expected edge backend -> backend/api, got %+v", resp.Edges)
	}
	if !hasEdge(resp, "backend", "backend/store") {
		t.Fatalf("expected edge backend -> backend/store, got %+v", resp.Edges)
	}

	deps, err := architecture.NewServiceDependencies(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range deps.Dependencies() {
		if !hasEdge(resp, dep.From.Unique, dep.To.Unique) {
			t.Fatalf("expected service-dependency edge %s -> %s, got %+v", dep.From.Unique, dep.To.Unique, resp.Edges)
		}
	}
}

// TestInventoryUsesServerWorkspaceNotCwd uses a module-free workspace:
// GetWorkspaceInventory walks through architecture.LoadWorkspace, which spins
// up each service's real agent runtime, so a fixture with services would need
// real installed agent binaries. The module list is irrelevant to what this
// test checks (workspace identity, not service loading).
func TestInventoryUsesServerWorkspaceNotCwd(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspace.codefly.yaml")
	if err := os.WriteFile(path, []byte("name: demo\nlayout: flat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0", EndpointRest: "127.0.0.1:0"}, ws, nil)
	if err != nil {
		t.Fatal(err)
	}

	inv, err := server.GetWorkspaceInventory(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Name != "demo" {
		t.Fatalf("expected inventory for server workspace %q, got %q", "demo", inv.Name)
	}
}

func logAt(at time.Time, message string) *observabilityv0.Log {
	return &observabilityv0.Log{At: timestamppb.New(at), Message: message}
}

func messages(logs []*observabilityv0.Log) []string {
	var out []string
	for _, l := range logs {
		out = append(out, l.Message)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLogHistoryRoundTrip(t *testing.T) {
	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0", EndpointRest: "127.0.0.1:0"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	server.recordLog(logAt(t0, "one"))
	server.recordLog(logAt(t0.Add(time.Second), "two"))
	server.recordLog(logAt(t0.Add(2*time.Second), "three"))

	resp, err := server.ActiveLogHistory(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := messages(resp.Groups[0].Logs)
	if want := []string{"one", "two", "three"}; !equalStrings(got, want) {
		t.Fatalf("ActiveLogHistory(nil) = %v, want %v", got, want)
	}

	resp, err = server.ActiveLogHistory(context.Background(), &observabilityv0.LogRequest{From: timestamppb.New(t0.Add(time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	got = messages(resp.Groups[0].Logs)
	if want := []string{"two", "three"}; !equalStrings(got, want) {
		t.Fatalf("ActiveLogHistory(From=t0+1s) = %v, want %v", got, want)
	}

	logResp, err := server.LogHistory(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(messages(logResp.Groups[0].Logs), []string{"one", "two", "three"}) {
		t.Fatalf("LogHistory diverged from ActiveLogHistory: %v", messages(logResp.Groups[0].Logs))
	}
}

func TestLogHistoryIsBounded(t *testing.T) {
	h := newLogHistory(3)
	t0 := time.Now()
	for i := 1; i <= 5; i++ {
		h.Add(logAt(t0.Add(time.Duration(i)*time.Second), string(rune('0'+i))))
	}
	got := messages(h.Snapshot(nil, nil))
	if want := []string{"3", "4", "5"}; !equalStrings(got, want) {
		t.Fatalf("Snapshot = %v, want %v", got, want)
	}
}
