package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	codecore "github.com/codefly-dev/core/code"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// mockCodeClient implements codev0.CodeClient via the unified Execute RPC.
// Tests configure per-operation function fields; Execute dispatches to them.
type mockCodeClient struct {
	fixFn              func(ctx context.Context, in *codev0.FixRequest, opts ...grpc.CallOption) (*codev0.FixResponse, error)
	applyEditFn        func(ctx context.Context, in *codev0.ApplyEditRequest, opts ...grpc.CallOption) (*codev0.ApplyEditResponse, error)
	listDependenciesFn func(ctx context.Context, in *codev0.ListDependenciesRequest, opts ...grpc.CallOption) (*codev0.ListDependenciesResponse, error)
	addDependencyFn    func(ctx context.Context, in *codev0.AddDependencyRequest, opts ...grpc.CallOption) (*codev0.AddDependencyResponse, error)
	removeDependencyFn func(ctx context.Context, in *codev0.RemoveDependencyRequest, opts ...grpc.CallOption) (*codev0.RemoveDependencyResponse, error)
	getProjectInfoFn   func(ctx context.Context, in *codev0.GetProjectInfoRequest, opts ...grpc.CallOption) (*codev0.GetProjectInfoResponse, error)
}

type mockRuntimeClient struct {
	testFn func(ctx context.Context, in *runtimev0.TestRequest, opts ...grpc.CallOption) (*runtimev0.TestResponse, error)
}

func (m *mockCodeClient) Execute(ctx context.Context, in *codev0.CodeRequest, opts ...grpc.CallOption) (*codev0.CodeResponse, error) {
	switch op := in.Operation.(type) {
	case *codev0.CodeRequest_Fix:
		if m.fixFn == nil {
			return nil, fmt.Errorf("Fix not configured")
		}
		r, err := m.fixFn(ctx, op.Fix, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_Fix{Fix: r}}, nil
	case *codev0.CodeRequest_ApplyEdit:
		if m.applyEditFn == nil {
			return nil, fmt.Errorf("ApplyEdit not configured")
		}
		r, err := m.applyEditFn(ctx, op.ApplyEdit, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_ApplyEdit{ApplyEdit: r}}, nil
	case *codev0.CodeRequest_ListDependencies:
		if m.listDependenciesFn == nil {
			return nil, fmt.Errorf("ListDependencies not configured")
		}
		r, err := m.listDependenciesFn(ctx, op.ListDependencies, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_ListDependencies{ListDependencies: r}}, nil
	case *codev0.CodeRequest_AddDependency:
		if m.addDependencyFn == nil {
			return nil, fmt.Errorf("AddDependency not configured")
		}
		r, err := m.addDependencyFn(ctx, op.AddDependency, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_AddDependency{AddDependency: r}}, nil
	case *codev0.CodeRequest_RemoveDependency:
		if m.removeDependencyFn == nil {
			return nil, fmt.Errorf("RemoveDependency not configured")
		}
		r, err := m.removeDependencyFn(ctx, op.RemoveDependency, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_RemoveDependency{RemoveDependency: r}}, nil
	case *codev0.CodeRequest_GetProjectInfo:
		if m.getProjectInfoFn == nil {
			return nil, fmt.Errorf("GetProjectInfo not configured")
		}
		r, err := m.getProjectInfoFn(ctx, op.GetProjectInfo, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_GetProjectInfo{GetProjectInfo: r}}, nil
	default:
		return nil, fmt.Errorf("unknown operation %T", in.Operation)
	}
}

func (m *mockRuntimeClient) Load(context.Context, *runtimev0.LoadRequest, ...grpc.CallOption) (*runtimev0.LoadResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
}
func (m *mockRuntimeClient) Init(context.Context, *runtimev0.InitRequest, ...grpc.CallOption) (*runtimev0.InitResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
}
func (m *mockRuntimeClient) Start(context.Context, *runtimev0.StartRequest, ...grpc.CallOption) (*runtimev0.StartResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
}
func (m *mockRuntimeClient) Stop(context.Context, *runtimev0.StopRequest, ...grpc.CallOption) (*runtimev0.StopResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
}
func (m *mockRuntimeClient) Destroy(context.Context, *runtimev0.DestroyRequest, ...grpc.CallOption) (*runtimev0.DestroyResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
}
func (m *mockRuntimeClient) Build(context.Context, *runtimev0.BuildRequest, ...grpc.CallOption) (*runtimev0.BuildResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
}
func (m *mockRuntimeClient) Test(ctx context.Context, in *runtimev0.TestRequest, opts ...grpc.CallOption) (*runtimev0.TestResponse, error) {
	if m.testFn == nil {
		return nil, fmt.Errorf("Test not configured")
	}
	return m.testFn(ctx, in, opts...)
}
func (m *mockRuntimeClient) Lint(context.Context, *runtimev0.LintRequest, ...grpc.CallOption) (*runtimev0.LintResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
}
func (m *mockRuntimeClient) Information(context.Context, *runtimev0.InformationRequest, ...grpc.CallOption) (*runtimev0.InformationResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
}
func (m *mockRuntimeClient) Communicate(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentv0.Answer, agentv0.Question], error) {
	return nil, fmt.Errorf("not exercised in mock")
}

// newTestServer creates a Server with the mock injected into the plugins map.
// The service name "test-svc" is used throughout, matching the mindYAML config.
func newTestServer(mock codev0.CodeClient) *Server {
	return newTestServerWithWorkDir(mock, os.TempDir())
}

func newTestServerWithWorkDir(mock codev0.CodeClient, workDir string) *Server {
	s := &Server{
		cfg: Config{
			WorkDir: workDir,
			Port:    0,
		},
		mindYAML: &MindYAML{
			Service: "test-svc",
			Plugin:  "generic-go",
			Config: SvcConfig{
				Path: ".",
				Type: "go",
			},
		},
		plugins:    make(map[string]*pluginConn),
		stopHealth: make(chan struct{}),
	}
	s.plugins["test-svc"] = &pluginConn{
		code: mock,
	}
	return s
}

func newTestServerWithRuntime(runtime runtimev0.RuntimeClient) *Server {
	s := newTestServer(&mockCodeClient{})
	s.plugins["test-svc"].runtime = runtime
	return s
}

func editingMock(t *testing.T, dir string) *mockCodeClient {
	t.Helper()
	return &mockCodeClient{applyEditFn: func(_ context.Context, in *codev0.ApplyEditRequest, _ ...grpc.CallOption) (*codev0.ApplyEditResponse, error) {
		path := filepath.Join(dir, filepath.Clean(in.GetFile()))
		original, err := os.ReadFile(path)
		if err != nil {
			return &codev0.ApplyEditResponse{Success: false}, nil
		}
		if strings.Count(string(original), in.GetFind()) != 1 {
			return &codev0.ApplyEditResponse{Success: false}, nil
		}
		content := strings.Replace(string(original), in.GetFind(), in.GetReplace(), 1)
		if !in.GetDryRun() {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return nil, err
			}
		}
		return &codev0.ApplyEditResponse{
			Success: true, Content: content, Strategy: "exact", Changed: content != string(original), Wrote: !in.GetDryRun(),
		}, nil
	}}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSubscribeWorkspaceChangesStreamsExternalEditsAndReplaysReconnect(t *testing.T) {
	root := t.TempDir()
	srv, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.CloseWorkspaceChanges() })

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	gatewayv1.RegisterGatewayServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient("passthrough:///workspace-watch", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := gatewayv1.NewGatewayClient(connection)

	firstContext, cancelFirst := context.WithTimeout(t.Context(), 5*time.Second)
	first, err := client.SubscribeWorkspaceChanges(firstContext, &gatewayv1.SubscribeWorkspaceChangesRequest{})
	if err != nil {
		cancelFirst()
		t.Fatal(err)
	}
	firstResult := make(chan gatewayWorkspaceReceiveResult, 1)
	go func() {
		event, receiveErr := receiveGatewayWorkspacePath(first, "a.go")
		firstResult <- gatewayWorkspaceReceiveResult{event: event, err: receiveErr}
	}()
	waitForWorkspaceMonitor(t, srv)
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		cancelFirst()
		t.Fatal(err)
	}
	firstReceived := <-firstResult
	if firstReceived.err != nil {
		cancelFirst()
		t.Fatalf("receive workspace change %q: %v", "a.go", firstReceived.err)
	}
	firstEvent := firstReceived.event
	cancelFirst()

	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Let the shared Codefly monitor retain the disconnected event before the
	// client presents its durable cursor.
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.workspaceChangesMu.Lock()
		var cursor codecore.WorkspaceChangeCursor
		if srv.workspaceChanges != nil {
			cursor = srv.workspaceChanges.Cursor()
		}
		srv.workspaceChangesMu.Unlock()
		if cursor.Sequence > firstEvent.GetSequence() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workspace monitor did not retain disconnected edit")
		}
		time.Sleep(10 * time.Millisecond)
	}

	secondContext, cancelSecond := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelSecond()
	second, err := client.SubscribeWorkspaceChanges(secondContext, &gatewayv1.SubscribeWorkspaceChangesRequest{After: &gatewayv1.WorkspaceChangeCursor{
		SourceId: firstEvent.GetSourceId(), Sequence: firstEvent.GetSequence(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	secondEvent, err := receiveGatewayWorkspacePath(second, "b.go")
	if err != nil {
		t.Fatal(err)
	}
	if secondEvent.GetSourceId() != firstEvent.GetSourceId() || secondEvent.GetSequence() <= firstEvent.GetSequence() {
		t.Fatalf("replayed event=%+v after=%+v", secondEvent, firstEvent)
	}

	foreignContext, cancelForeign := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelForeign()
	foreign, err := client.SubscribeWorkspaceChanges(foreignContext, &gatewayv1.SubscribeWorkspaceChangesRequest{After: &gatewayv1.WorkspaceChangeCursor{
		SourceId: "previous-gateway-process", Sequence: 10,
	}})
	if err != nil {
		t.Fatal(err)
	}
	foreignEvent, err := foreign.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(foreignEvent.GetChanges()) != 1 || foreignEvent.GetChanges()[0].GetOperation() != gatewayv1.WorkspaceChangeOperation_WORKSPACE_CHANGE_OPERATION_RESCAN || foreignEvent.GetChanges()[0].GetReason() != "source_changed" {
		t.Fatalf("foreign cursor event=%+v", foreignEvent)
	}
}

type gatewayWorkspaceReceiveResult struct {
	event *gatewayv1.WorkspaceChangeEvent
	err   error
}

func receiveGatewayWorkspacePath(stream gatewayv1.Gateway_SubscribeWorkspaceChangesClient, path string) (*gatewayv1.WorkspaceChangeEvent, error) {
	for {
		event, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		for _, change := range event.GetChanges() {
			if change.GetPath() == path {
				return event, nil
			}
		}
	}
}

func waitForWorkspaceMonitor(t *testing.T, server *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		server.workspaceChangesMu.Lock()
		ready := server.workspaceChanges != nil
		server.workspaceChangesMu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("workspace monitor was not initialized")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTestPreservesStructuredRuntimeFields(t *testing.T) {
	rt := &mockRuntimeClient{
		testFn: func(_ context.Context, _ *runtimev0.TestRequest, _ ...grpc.CallOption) (*runtimev0.TestResponse, error) {
			return &runtimev0.TestResponse{
				Output: "raw output",
				Result: &runtimev0.TestRunResult{
					State:   runtimev0.TestRunResult_FAILED,
					Message: "suite failed",
				},
				Counts: &runtimev0.TestCounts{
					Total:   3,
					Passed:  1,
					Failed:  1,
					Skipped: 1,
				},
				Coverage: &runtimev0.TestCoverage{TotalPct: 87.5},
				Suites: []*runtimev0.TestSuite{
					{
						Cases: []*runtimev0.TestCase{
							{
								FullName: "pkg.TestFails",
								Failure:  &runtimev0.TestFailure{Message: "want 1 got 2"},
							},
						},
					},
				},
			}, nil
		},
	}
	s := newTestServerWithRuntime(rt)

	resp, err := s.Test(context.Background(), &gatewayv1.TestRequest{})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.Success {
		t.Fatal("Success = true, want false")
	}
	if resp.Output != "suite failed\nraw output" {
		t.Fatalf("Output = %q", resp.Output)
	}
	if resp.TestsRun != 3 || resp.TestsPassed != 1 || resp.TestsFailed != 1 || resp.TestsSkipped != 1 {
		t.Fatalf("counts = run:%d passed:%d failed:%d skipped:%d", resp.TestsRun, resp.TestsPassed, resp.TestsFailed, resp.TestsSkipped)
	}
	if resp.CoveragePct != 87.5 {
		t.Fatalf("CoveragePct = %v", resp.CoveragePct)
	}
	if len(resp.Failures) != 1 || resp.Failures[0] != "pkg.TestFails: want 1 got 2" {
		t.Fatalf("Failures = %#v", resp.Failures)
	}
}

func TestFix(t *testing.T) {
	mock := &mockCodeClient{
		fixFn: func(_ context.Context, in *codev0.FixRequest, _ ...grpc.CallOption) (*codev0.FixResponse, error) {
			if in.File != "main.go" {
				t.Errorf("expected file main.go, got %s", in.File)
			}
			if in.GetMode() != basev0.FixMode_FIX_MODE_AGGRESSIVE || !in.GetDryRun() {
				t.Errorf("gateway did not preserve fix mode/dry-run: %+v", in)
			}
			return &codev0.FixResponse{
				Success: true, Changed: true, Wrote: false,
				Content:      "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }",
				Actions:      []string{"goimports", "gofmt"},
				BeforeSha256: "before", AfterSha256: "after",
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.Fix(context.Background(), &gatewayv1.FixRequest{
		Path: "main.go", Mode: basev0.FixMode_FIX_MODE_AGGRESSIVE, DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if len(resp.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(resp.Actions))
	}
	if resp.Actions[0] != "goimports" {
		t.Errorf("expected first action 'goimports', got %s", resp.Actions[0])
	}
	if !resp.GetChanged() || resp.GetWrote() || resp.GetBeforeSha256() != "before" || resp.GetAfterSha256() != "after" {
		t.Fatalf("gateway dropped fix evidence: %+v", resp)
	}
}

func TestApplyEdit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nold code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithWorkDir(editingMock(t, dir), dir)
	resp, err := s.ApplyEdit(context.Background(), &gatewayv1.ApplyEditRequest{
		File: "main.go", Find: "old code", Replace: "new code", FixMode: basev0.FixMode_FIX_MODE_SAFE,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if resp.Strategy != "exact" {
		t.Errorf("expected strategy 'exact', got %s", resp.Strategy)
	}
	content, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != "package main\n\nnew code\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestDirectWorkspaceFileOperations(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc needle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "note.txt"), []byte("needle in text\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithWorkDir(editingMock(t, dir), dir)

	writeResp, err := s.WriteFile(context.Background(), &gatewayv1.WriteFileRequest{
		Service: "test-svc",
		Path:    "nested/new.txt",
		Content: "created by gateway",
	})
	if err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if !writeResp.Success {
		t.Fatalf("WriteFile failed: %s", writeResp.Error)
	}

	readResp, err := s.ReadFile(context.Background(), &gatewayv1.ReadFileRequest{
		Service: "test-svc",
		Path:    "nested/new.txt",
	})
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !readResp.Exists || readResp.Content != "created by gateway" {
		t.Fatalf("ReadFile = exists:%v content:%q", readResp.Exists, readResp.Content)
	}

	createResp, err := s.CreateFile(context.Background(), &gatewayv1.CreateFileRequest{
		Service:   "test-svc",
		Path:      "nested/new.txt",
		Content:   "replacement",
		Overwrite: false,
	})
	if err != nil {
		t.Fatalf("CreateFile returned error: %v", err)
	}
	if createResp.Success {
		t.Fatal("CreateFile overwrote an existing file with overwrite=false")
	}

	listResp, err := s.ListFiles(context.Background(), &gatewayv1.ListFilesRequest{
		Service:    "test-svc",
		Path:       ".",
		Recursive:  true,
		Extensions: []string{".go"},
	})
	if err != nil {
		t.Fatalf("ListFiles returned error: %v", err)
	}
	if !fileInfoContains(listResp.Files, "main.go") {
		t.Fatalf("ListFiles did not include main.go: %#v", listResp.Files)
	}

	searchResp, err := s.Search(context.Background(), &gatewayv1.SearchRequest{
		Service:    "test-svc",
		Pattern:    "needle",
		Literal:    true,
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if searchResp.TotalMatches < 2 {
		t.Fatalf("Search TotalMatches = %d, want at least 2", searchResp.TotalMatches)
	}

	editResp, err := s.ApplyEdit(context.Background(), &gatewayv1.ApplyEditRequest{
		Service: "test-svc",
		File:    "main.go",
		Find:    "func needle() {}",
		Replace: "func renamed() {}",
	})
	if err != nil {
		t.Fatalf("ApplyEdit returned error: %v", err)
	}
	if !editResp.Success {
		t.Fatalf("ApplyEdit failed: %s", editResp.Error)
	}

	moveResp, err := s.MoveFile(context.Background(), &gatewayv1.MoveFileRequest{
		Service: "test-svc",
		OldPath: "nested/new.txt",
		NewPath: "nested/moved.txt",
	})
	if err != nil {
		t.Fatalf("MoveFile returned error: %v", err)
	}
	if !moveResp.Success {
		t.Fatalf("MoveFile failed: %s", moveResp.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested", "moved.txt")); err != nil {
		t.Fatalf("moved file not present: %v", err)
	}

	deleteResp, err := s.DeleteFile(context.Background(), &gatewayv1.DeleteFileRequest{
		Service: "test-svc",
		Path:    "nested/moved.txt",
	})
	if err != nil {
		t.Fatalf("DeleteFile returned error: %v", err)
	}
	if !deleteResp.Success {
		t.Fatalf("DeleteFile failed: %s", deleteResp.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested", "moved.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file still exists or stat failed unexpectedly: %v", err)
	}
}

func fileInfoContains(files []*gatewayv1.FileInfo, path string) bool {
	for _, file := range files {
		if file.GetPath() == path {
			return true
		}
	}
	return false
}

func TestDirectWorkspaceFileOperationsRejectPathEscape(t *testing.T) {
	s := newTestServerWithWorkDir(&mockCodeClient{}, t.TempDir())
	_, err := s.ReadFile(context.Background(), &gatewayv1.ReadFileRequest{
		Service: "test-svc",
		Path:    "../outside.txt",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ReadFile path escape code = %s, want %s (err=%v)", status.Code(err), codes.InvalidArgument, err)
	}
}

func TestDirectWorkspaceFileOperationsRejectUnknownService(t *testing.T) {
	s := newTestServerWithWorkDir(&mockCodeClient{}, t.TempDir())
	_, err := s.ReadFile(context.Background(), &gatewayv1.ReadFileRequest{
		Service: "other",
		Path:    "main.go",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("ReadFile unknown service code = %s, want %s (err=%v)", status.Code(err), codes.NotFound, err)
	}
}

func TestListDependencies(t *testing.T) {
	mock := &mockCodeClient{
		listDependenciesFn: func(_ context.Context, _ *codev0.ListDependenciesRequest, _ ...grpc.CallOption) (*codev0.ListDependenciesResponse, error) {
			return &codev0.ListDependenciesResponse{
				Dependencies: []*codev0.Dependency{
					{Name: "github.com/gin-gonic/gin", Version: "v1.9.1", Direct: true},
					{Name: "golang.org/x/sys", Version: "v0.15.0", Direct: false},
				},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.ListDependencies(context.Background(), &gatewayv1.ListDependenciesRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != "" {
		t.Errorf("unexpected error: %s", resp.Error)
	}
	if len(resp.Dependencies) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(resp.Dependencies))
	}
	if resp.Dependencies[0].Name != "github.com/gin-gonic/gin" {
		t.Errorf("expected gin dependency, got %s", resp.Dependencies[0].Name)
	}
	if !resp.Dependencies[0].Direct {
		t.Error("expected gin to be direct")
	}
	if resp.Dependencies[1].Direct {
		t.Error("expected x/sys to be indirect")
	}
}

func TestGitStatus(t *testing.T) {
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	run("config", "commit.gpgsign", "false")

	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "committed.txt")
	run("commit", "-m", "initial")

	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stage a modification to make it reliably visible in git status,
	// avoiding stat-cache race conditions with unstaged changes.
	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "committed.txt")

	s := &Server{
		cfg:      Config{WorkDir: dir},
		mindYAML: &MindYAML{Service: "test-svc", Plugin: "generic-go"},
		plugins:  make(map[string]*pluginConn),
	}
	resp, err := s.GitStatus(context.Background(), &gatewayv1.GitStatusRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.Branch == "" {
		t.Error("expected a branch name")
	}
	if len(resp.Files) < 2 {
		t.Fatalf("expected at least 2 file statuses, got %d", len(resp.Files))
	}

	found := map[string]string{}
	staged := map[string]bool{}
	for _, f := range resp.Files {
		found[f.Path] = f.Status
		staged[f.Path] = f.Staged
	}
	if status, ok := found["committed.txt"]; !ok {
		t.Errorf("expected committed.txt in git status, got files: %v", found)
	} else if status != "modified" {
		t.Errorf("expected committed.txt status 'modified', got %s", status)
	}
	if !staged["committed.txt"] {
		t.Error("expected committed.txt to be staged")
	}
	if status, ok := found["untracked.txt"]; !ok {
		t.Error("expected untracked.txt in git status")
	} else if status != "untracked" {
		t.Errorf("expected untracked.txt status 'untracked', got %s", status)
	}
}

func TestRunCommand(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithWorkDir(editingMock(t, dir), dir)

	resp, err := s.RunCommand(context.Background(), &gatewayv1.RunCommandRequest{
		Command: "echo",
		Args:    []string{"hello", "world"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", resp.ExitCode)
	}
	stdout := strings.TrimSpace(resp.Stdout)
	if stdout != "hello world" {
		t.Errorf("expected stdout 'hello world', got %q", stdout)
	}
}

func TestRunCommand_Failure(t *testing.T) {
	dir := t.TempDir()
	s := newTestServerWithWorkDir(&mockCodeClient{}, dir)

	resp, err := s.RunCommand(context.Background(), &gatewayv1.RunCommandRequest{
		Command: "sh",
		Args:    []string{"-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", resp.ExitCode)
	}
}

func TestListServices(t *testing.T) {
	s := newTestServer(&mockCodeClient{})
	resp, err := s.ListServices(context.Background(), &gatewayv1.ListServicesRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(resp.Services))
	}
	svc := resp.Services[0]
	if svc.Name != "test-svc" {
		t.Errorf("expected service name 'test-svc', got %s", svc.Name)
	}
	if svc.Language != "go" {
		t.Errorf("expected language 'go', got %s", svc.Language)
	}
}

func TestAddDependency(t *testing.T) {
	mock := &mockCodeClient{
		addDependencyFn: func(_ context.Context, in *codev0.AddDependencyRequest, _ ...grpc.CallOption) (*codev0.AddDependencyResponse, error) {
			return &codev0.AddDependencyResponse{
				Success:          true,
				InstalledVersion: "v1.9.1",
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.AddDependency(context.Background(), &gatewayv1.AddDependencyRequest{
		PackageName: "github.com/gin-gonic/gin",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if resp.InstalledVersion != "v1.9.1" {
		t.Errorf("expected installed version v1.9.1, got %s", resp.InstalledVersion)
	}
}

func TestRemoveDependency(t *testing.T) {
	mock := &mockCodeClient{
		removeDependencyFn: func(_ context.Context, in *codev0.RemoveDependencyRequest, _ ...grpc.CallOption) (*codev0.RemoveDependencyResponse, error) {
			if in.PackageName != "github.com/old/dep" {
				t.Errorf("expected package 'github.com/old/dep', got %s", in.PackageName)
			}
			return &codev0.RemoveDependencyResponse{Success: true}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.RemoveDependency(context.Background(), &gatewayv1.RemoveDependencyRequest{
		PackageName: "github.com/old/dep",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
}

func TestBatchApplyEdits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("old1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("old2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithWorkDir(editingMock(t, dir), dir)
	resp, err := s.BatchApplyEdits(context.Background(), &gatewayv1.BatchApplyEditsRequest{
		Edits: []*gatewayv1.ApplyEditRequest{
			{File: "a.go", Find: "old1", Replace: "new1"},
			{File: "b.go", Find: "old2", Replace: "new2"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Succeeded != 2 {
		t.Errorf("expected 2 succeeded, got %d", resp.Succeeded)
	}
	if resp.Failed != 0 {
		t.Errorf("expected 0 failed, got %d", resp.Failed)
	}
	a, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "b.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != "new1\n" || string(b) != "new2\n" {
		t.Fatalf("batch content = a:%q b:%q", a, b)
	}
}

func TestBatchApplyEditsAbortsWithoutPartialWrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("old1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("old2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithWorkDir(editingMock(t, dir), dir)
	resp, err := s.BatchApplyEdits(context.Background(), &gatewayv1.BatchApplyEditsRequest{Edits: []*gatewayv1.ApplyEditRequest{
		{File: "a.go", Find: "old1", Replace: "new1"},
		{File: "b.go", Find: "missing", Replace: "new2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSucceeded() != 0 || resp.GetFailed() != 2 {
		t.Fatalf("batch counts = succeeded:%d failed:%d", resp.GetSucceeded(), resp.GetFailed())
	}
	a, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	b, _ := os.ReadFile(filepath.Join(dir, "b.go"))
	if string(a) != "old1\n" || string(b) != "old2\n" {
		t.Fatalf("failed atomic batch changed files: a=%q b=%q", a, b)
	}
}

func TestRunCommand_WorkingDir(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "marker.txt"), []byte("found"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithWorkDir(&mockCodeClient{}, dir)
	resp, err := s.RunCommand(context.Background(), &gatewayv1.RunCommandRequest{
		Command:    "ls",
		WorkingDir: "sub",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d (stderr: %s)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "marker.txt") {
		t.Errorf("expected stdout to contain 'marker.txt', got %q", resp.Stdout)
	}
}

func TestRunCommandRejectsEscapingWorkingDir(t *testing.T) {
	root := t.TempDir()
	s := newTestServerWithWorkDir(&mockCodeClient{}, root)
	for _, workDir := range []string{"../outside", "/tmp"} {
		_, err := s.RunCommand(context.Background(), &gatewayv1.RunCommandRequest{Command: "pwd", WorkingDir: workDir})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("working_dir %q returned %v", workDir, err)
		}
	}

	outside := t.TempDir()
	link := filepath.Join(root, "escape-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := s.RunCommand(context.Background(), &gatewayv1.RunCommandRequest{Command: "pwd", WorkingDir: "escape-link"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("symlink escape returned %v", err)
	}
}

func TestRunCommandRejectsExcessiveTimeout(t *testing.T) {
	s := newTestServerWithWorkDir(&mockCodeClient{}, t.TempDir())
	_, err := s.RunCommand(context.Background(), &gatewayv1.RunCommandRequest{Command: "true", TimeoutSeconds: 601})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("timeout returned %v", err)
	}
}

func TestBoundedCommandBufferKeepsDrainingAndTruncates(t *testing.T) {
	b := newBoundedCommandBuffer()
	payload := []byte(strings.Repeat("x", maxGatewayCommandOutput+1024))
	n, err := b.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(payload))
	}
	out := b.Output()
	if !strings.Contains(out, "output truncated") {
		t.Fatal("truncation marker missing")
	}
	if len(out) > maxGatewayCommandOutput+100 {
		t.Fatalf("bounded output length = %d", len(out))
	}
}

func TestGitRPCInputBoundsAndOptionBoundary(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	run("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithWorkDir(&mockCodeClient{}, dir)

	if _, err := s.GitDiff(context.Background(), &gatewayv1.GitDiffRequest{Path: "../outside"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("GitDiff traversal returned %v", err)
	}
	if _, err := s.GitLog(context.Background(), &gatewayv1.GitLogRequest{Count: 1001}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("GitLog excessive count returned %v", err)
	}

	// "--all" is a valid filename but must be placed after git's -- option
	// boundary. The old argv interpreted it as an option and staged every file.
	resp, err := s.GitCommit(context.Background(), &gatewayv1.GitCommitRequest{Paths: []string{"--all"}, Message: "should not commit"})
	if err != nil {
		t.Fatalf("GitCommit: %v", err)
	}
	if resp.Success {
		t.Fatal("option-like path was interpreted as git add --all")
	}
	if statusOut := run("status", "--porcelain=v1"); !strings.Contains(statusOut, "?? untracked.txt") {
		t.Fatalf("untracked file was unexpectedly staged: %q", statusOut)
	}
}

func TestPluginToAgentName(t *testing.T) {
	tests := []struct {
		plugin string
		want   string
	}{
		// Canonical names (new).
		{"go-generic", "go-generic:latest"},
		{"rust-generic", "rust-generic:latest"},
		{"node-generic", "node-generic:latest"},
		{"python-generic", "python-generic:latest"},
		// Legacy names (backward compat).
		{"generic-go", "go-generic:latest"},
		{"generic-rust", "rust-generic:latest"},
		{"generic-node", "node-generic:latest"},
		{"generic-python", "python-generic:latest"},
		// Unknown.
		{"custom-plugin", "custom-plugin:latest"},
	}
	for _, tt := range tests {
		got := pluginToAgentName(tt.plugin)
		if got != tt.want {
			t.Errorf("pluginToAgentName(%q) = %q, want %q", tt.plugin, got, tt.want)
		}
	}
}

func TestPluginToLang(t *testing.T) {
	tests := []struct {
		plugin string
		want   string
	}{
		// Canonical names (new).
		{"go-generic", "go"},
		{"rust-generic", "rust"},
		{"node-generic", "node"},
		{"python-generic", "python"},
		// Legacy names (backward compat).
		{"generic-go", "go"},
		{"generic-rust", "rust"},
		{"generic-node", "node"},
		{"generic-python", "python"},
		// Unknown.
		{"unknown", "unknown"},
	}
	for _, tt := range tests {
		got := pluginToLang(tt.plugin)
		if got != tt.want {
			t.Errorf("pluginToLang(%q) = %q, want %q", tt.plugin, got, tt.want)
		}
	}
}

func TestGitStatusString(t *testing.T) {
	tests := []struct {
		xy   string
		want string
	}{
		{"??", "untracked"},
		{"A ", "added"},
		{" A", "added"},
		{"D ", "deleted"},
		{" D", "deleted"},
		{"R ", "renamed"},
		{"M ", "modified"},
		{" M", "modified"},
	}
	for _, tt := range tests {
		got := gitStatusString(tt.xy)
		if got != tt.want {
			t.Errorf("gitStatusString(%q) = %q, want %q", tt.xy, got, tt.want)
		}
	}
}

func TestConfigBindHostDefaultsToLoopback(t *testing.T) {
	// Empty Host = local-only, the safe default.
	if got := (Config{}).bindHost(); got != "127.0.0.1" {
		t.Fatalf("empty Host bindHost() = %q, want 127.0.0.1", got)
	}
	if got := (Config{Host: "  "}).bindHost(); got != "127.0.0.1" {
		t.Fatalf("blank Host bindHost() = %q, want 127.0.0.1", got)
	}
	// Explicit host is honored — 0.0.0.0 exposes the gateway for the
	// codefly-in-Docker data-plane model.
	if got := (Config{Host: "0.0.0.0"}).bindHost(); got != "0.0.0.0" {
		t.Fatalf("Host=0.0.0.0 bindHost() = %q, want 0.0.0.0", got)
	}
}

func TestNewServerRequiresTokenForNonLoopbackBind(t *testing.T) {
	if _, err := NewServer(Config{WorkDir: t.TempDir(), Host: "0.0.0.0"}); err == nil {
		t.Fatal("non-loopback gateway started without authentication")
	}
	if _, err := NewServer(Config{WorkDir: t.TempDir(), Host: "0.0.0.0", Token: "secret"}); err == nil {
		t.Fatal("non-loopback gateway started without TLS")
	}
	certFile, keyFile := writeTestTLSCertificate(t)
	if _, err := NewServer(Config{
		WorkDir: t.TempDir(), Host: "0.0.0.0", Token: "secret",
		TLSCertFile: certFile, TLSKeyFile: keyFile,
	}); err != nil {
		t.Fatalf("token-and-TLS-authenticated non-loopback gateway was rejected: %v", err)
	}
}

func TestNewServerTLSConfiguration(t *testing.T) {
	certFile, keyFile := writeTestTLSCertificate(t)
	if _, err := NewServer(Config{WorkDir: t.TempDir(), TLSCertFile: certFile}); err == nil {
		t.Fatal("gateway accepted a TLS certificate without a key")
	}
	if _, err := NewServer(Config{WorkDir: t.TempDir(), TLSClientCAFile: certFile}); err == nil {
		t.Fatal("gateway accepted a client CA without a server certificate")
	}

	s, err := NewServer(Config{
		WorkDir: t.TempDir(), TLSCertFile: certFile, TLSKeyFile: keyFile,
		TLSClientCAFile: certFile,
	})
	if err != nil {
		t.Fatalf("create mTLS gateway: %v", err)
	}
	if s.tlsConfig == nil || s.tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("client CA did not enable required client-certificate verification")
	}
}

func writeTestTLSCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "codefly-gateway-test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "gateway.crt")
	keyFile := filepath.Join(dir, "gateway.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestNewServerNormalizesWorkDirToAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s, err := NewServer(Config{WorkDir: "."})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(s.cfg.WorkDir) || s.cfg.WorkDir != dir {
		t.Fatalf("WorkDir = %q, want %q", s.cfg.WorkDir, dir)
	}
}

func TestAuthenticateGatewayRequest(t *testing.T) {
	if err := authenticateGatewayRequest(context.Background(), ""); err != nil {
		t.Fatalf("local tokenless mode should pass: %v", err)
	}
	if err := authenticateGatewayRequest(context.Background(), "secret"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token returned %v", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret"))
	if err := authenticateGatewayRequest(ctx, "secret"); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	headerCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-codefly-gateway-token", "secret"))
	if err := authenticateGatewayRequest(headerCtx, "secret"); err != nil {
		t.Fatalf("valid gateway header token rejected: %v", err)
	}
	bad := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer wrong"))
	if err := authenticateGatewayRequest(bad, "secret"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalid token returned %v", err)
	}
}
