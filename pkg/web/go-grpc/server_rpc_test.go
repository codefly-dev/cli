package go_grpc

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
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

// TestLogHistorySubscribeSnapshotExcludesFutureAdds pins down the atomicity
// Subscribe relies on: an entry added before Subscribe is only in the
// snapshot, and an entry added after is only on the live channel — never
// both, which is what made the dashboard's backfill-then-live-tail merge
// produce duplicate lines before Logs() started replaying through Subscribe.
func TestLogHistorySubscribeSnapshotExcludesFutureAdds(t *testing.T) {
	h := newLogHistory(10)
	h.Add(logAt(time.Now(), "past"))

	snapshot, live, unsubscribe := h.Subscribe(10)
	defer unsubscribe()

	if got := messages(snapshot); !equalStrings(got, []string{"past"}) {
		t.Fatalf("snapshot = %v, want [past]", got)
	}

	h.Add(logAt(time.Now(), "future"))

	select {
	case entry := <-live:
		if entry.Message != "future" {
			t.Fatalf("live delivered %q, want %q", entry.Message, "future")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live entry")
	}

	select {
	case entry := <-live:
		t.Fatalf("unexpected extra live entry (duplicate delivery): %+v", entry)
	default:
	}
}

func TestLogHistoryUnsubscribeStopsDelivery(t *testing.T) {
	h := newLogHistory(10)
	_, live, unsubscribe := h.Subscribe(10)
	unsubscribe()

	h.Add(logAt(time.Now(), "after-unsubscribe"))

	select {
	case entry, ok := <-live:
		if ok {
			t.Fatalf("expected no delivery after unsubscribe, got %+v", entry)
		}
	default:
	}
}

// fakeLogsServer implements cli.CLI_LogsServer without a real gRPC/Connect
// transport, so Server.Logs can be exercised directly.
type fakeLogsServer struct {
	ctx context.Context

	mu  sync.Mutex
	got []*observabilityv0.Log
}

func (f *fakeLogsServer) Send(entry *observabilityv0.Log) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, entry)
	return nil
}

func (f *fakeLogsServer) Context() context.Context     { return f.ctx }
func (f *fakeLogsServer) SetHeader(metadata.MD) error  { return nil }
func (f *fakeLogsServer) SendHeader(metadata.MD) error { return nil }
func (f *fakeLogsServer) SetTrailer(metadata.MD)       {}
func (f *fakeLogsServer) SendMsg(m any) error          { return nil }
func (f *fakeLogsServer) RecvMsg(m any) error          { return nil }

func (f *fakeLogsServer) snapshot() []*observabilityv0.Log {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*observabilityv0.Log, len(f.got))
	copy(out, f.got)
	return out
}

func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func subscriberCount(h *logHistory) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}

// TestLogsReplaysHistoryThenLiveExactlyOnce is the regression test for the
// dashboard's duplicate-log bug: before this fix, the dashboard combined a
// point-in-time ActiveLogHistory snapshot with an independently-subscribed
// live stream, so any line recorded in the race window between the two
// appeared twice. Logs() now replays history and subscribes to live entries
// atomically, so a single stream carries every entry exactly once.
func TestLogsReplaysHistoryThenLiveExactlyOnce(t *testing.T) {
	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0", EndpointRest: "127.0.0.1:0"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	server.recordLog(logAt(t0, "before-subscribe"))

	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeLogsServer{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- server.Logs(&emptypb.Empty{}, fake) }()

	waitFor(t, func() bool { return subscriberCount(server.history) >= 1 }, "Logs() to subscribe")
	server.recordLog(logAt(t0.Add(time.Second), "after-subscribe"))
	waitFor(t, func() bool { return len(fake.snapshot()) >= 2 }, "both log lines to arrive")

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	got := messages(fake.snapshot())
	if want := []string{"before-subscribe", "after-subscribe"}; !equalStrings(got, want) {
		t.Fatalf("Logs() delivered %v, want %v exactly once each, in order", got, want)
	}
}

// TestLogsFansOutToEachConcurrentSubscriber is the regression test for the
// single-shared-channel design: before this fix, s.logChannel was drained by
// whichever Logs() call's goroutine won the race for each entry, so a second
// concurrently open dashboard tab silently received only part of the live
// feed. Each Logs() call now gets its own subscriber channel from logHistory,
// so every concurrent viewer sees every entry.
func TestLogsFansOutToEachConcurrentSubscriber(t *testing.T) {
	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0", EndpointRest: "127.0.0.1:0"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	fake1 := &fakeLogsServer{ctx: ctx1}
	fake2 := &fakeLogsServer{ctx: ctx2}

	done1 := make(chan error, 1)
	done2 := make(chan error, 1)
	go func() { done1 <- server.Logs(&emptypb.Empty{}, fake1) }()
	go func() { done2 <- server.Logs(&emptypb.Empty{}, fake2) }()

	waitFor(t, func() bool { return subscriberCount(server.history) >= 2 }, "both Logs() calls to subscribe")
	server.recordLog(logAt(time.Now(), "broadcast"))
	waitFor(t, func() bool { return len(fake1.snapshot()) >= 1 }, "subscriber 1 to receive the broadcast entry")
	waitFor(t, func() bool { return len(fake2.snapshot()) >= 1 }, "subscriber 2 to receive the broadcast entry")

	cancel1()
	cancel2()
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	if err := <-done2; err != nil {
		t.Fatal(err)
	}

	if got := messages(fake1.snapshot()); !equalStrings(got, []string{"broadcast"}) {
		t.Fatalf("subscriber 1 got %v, want [broadcast]", got)
	}
	if got := messages(fake2.snapshot()); !equalStrings(got, []string{"broadcast"}) {
		t.Fatalf("subscriber 2 got %v, want [broadcast]", got)
	}
}
