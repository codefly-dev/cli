package gateway

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"google.golang.org/grpc"
)

// mockCodeClient implements codev0.CodeClient via the unified Execute RPC.
// Tests configure per-operation function fields; Execute dispatches to them.
type mockCodeClient struct {
	listSymbolsFn      func(ctx context.Context, in *codev0.ListSymbolsRequest, opts ...grpc.CallOption) (*codev0.ListSymbolsResponse, error)
	getDiagnosticsFn   func(ctx context.Context, in *codev0.GetDiagnosticsRequest, opts ...grpc.CallOption) (*codev0.GetDiagnosticsResponse, error)
	goToDefinitionFn   func(ctx context.Context, in *codev0.GoToDefinitionRequest, opts ...grpc.CallOption) (*codev0.GoToDefinitionResponse, error)
	findReferencesFn   func(ctx context.Context, in *codev0.FindReferencesRequest, opts ...grpc.CallOption) (*codev0.FindReferencesResponse, error)
	renameSymbolFn     func(ctx context.Context, in *codev0.RenameSymbolRequest, opts ...grpc.CallOption) (*codev0.RenameSymbolResponse, error)
	getHoverInfoFn     func(ctx context.Context, in *codev0.GetHoverInfoRequest, opts ...grpc.CallOption) (*codev0.GetHoverInfoResponse, error)
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
	case *codev0.CodeRequest_ListSymbols:
		if m.listSymbolsFn == nil {
			return nil, fmt.Errorf("ListSymbols not configured")
		}
		r, err := m.listSymbolsFn(ctx, op.ListSymbols, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_ListSymbols{ListSymbols: r}}, nil
	case *codev0.CodeRequest_GetDiagnostics:
		if m.getDiagnosticsFn == nil {
			return nil, fmt.Errorf("GetDiagnostics not configured")
		}
		r, err := m.getDiagnosticsFn(ctx, op.GetDiagnostics, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_GetDiagnostics{GetDiagnostics: r}}, nil
	case *codev0.CodeRequest_GoToDefinition:
		if m.goToDefinitionFn == nil {
			return nil, fmt.Errorf("GoToDefinition not configured")
		}
		r, err := m.goToDefinitionFn(ctx, op.GoToDefinition, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_GoToDefinition{GoToDefinition: r}}, nil
	case *codev0.CodeRequest_FindReferences:
		if m.findReferencesFn == nil {
			return nil, fmt.Errorf("FindReferences not configured")
		}
		r, err := m.findReferencesFn(ctx, op.FindReferences, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_FindReferences{FindReferences: r}}, nil
	case *codev0.CodeRequest_RenameSymbol:
		if m.renameSymbolFn == nil {
			return nil, fmt.Errorf("RenameSymbol not configured")
		}
		r, err := m.renameSymbolFn(ctx, op.RenameSymbol, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_RenameSymbol{RenameSymbol: r}}, nil
	case *codev0.CodeRequest_GetHoverInfo:
		if m.getHoverInfoFn == nil {
			return nil, fmt.Errorf("GetHoverInfo not configured")
		}
		r, err := m.getHoverInfoFn(ctx, op.GetHoverInfo, opts...)
		if err != nil {
			return nil, err
		}
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_GetHoverInfo{GetHoverInfo: r}}, nil
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

// Per-RPC stubs satisfy the CodeClient interface for the RPCs that still
// exist post-cleanup (file/git/search/rename/fix/deps/project-info RPCs
// were deleted from service Code — everything dispatches through Execute).
// These stubs return errors because tests only exercise the Execute path.
func (m *mockCodeClient) ListSymbols(_ context.Context, _ *codev0.ListSymbolsRequest, _ ...grpc.CallOption) (*codev0.ListSymbolsResponse, error) {
	return nil, fmt.Errorf("not exercised in mock; use Execute")
}
func (m *mockCodeClient) GetDiagnostics(_ context.Context, _ *codev0.GetDiagnosticsRequest, _ ...grpc.CallOption) (*codev0.GetDiagnosticsResponse, error) {
	return nil, fmt.Errorf("not exercised in mock; use Execute")
}
func (m *mockCodeClient) GoToDefinition(_ context.Context, _ *codev0.GoToDefinitionRequest, _ ...grpc.CallOption) (*codev0.GoToDefinitionResponse, error) {
	return nil, fmt.Errorf("not exercised in mock; use Execute")
}
func (m *mockCodeClient) FindReferences(_ context.Context, _ *codev0.FindReferencesRequest, _ ...grpc.CallOption) (*codev0.FindReferencesResponse, error) {
	return nil, fmt.Errorf("not exercised in mock; use Execute")
}
func (m *mockCodeClient) GetHoverInfo(_ context.Context, _ *codev0.GetHoverInfoRequest, _ ...grpc.CallOption) (*codev0.GetHoverInfoResponse, error) {
	return nil, fmt.Errorf("not exercised in mock; use Execute")
}
func (m *mockCodeClient) ApplyEdit(_ context.Context, _ *codev0.ApplyEditRequest, _ ...grpc.CallOption) (*codev0.ApplyEditResponse, error) {
	return nil, fmt.Errorf("not exercised in mock; use Execute")
}
func (m *mockCodeClient) GetCallGraph(_ context.Context, _ *codev0.GetCallGraphRequest, _ ...grpc.CallOption) (*codev0.GetCallGraphResponse, error) {
	return nil, fmt.Errorf("not exercised in mock; use Tooling.GetCallGraph")
}
func (m *mockCodeClient) ShellExec(_ context.Context, _ *codev0.ShellExecRequest, _ ...grpc.CallOption) (*codev0.ShellExecResponse, error) {
	return nil, fmt.Errorf("not exercised in mock")
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

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

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

func TestListSymbols_Success(t *testing.T) {
	mock := &mockCodeClient{
		listSymbolsFn: func(_ context.Context, in *codev0.ListSymbolsRequest, _ ...grpc.CallOption) (*codev0.ListSymbolsResponse, error) {
			if in.File != "main.go" {
				t.Errorf("expected file main.go, got %s", in.File)
			}
			return &codev0.ListSymbolsResponse{
				Symbols: []*codev0.Symbol{
					{
						Name:      "main",
						Kind:      codev0.SymbolKind_SYMBOL_KIND_FUNCTION,
						Signature: "func main()",
						Location:  &codev0.Location{File: "main.go", Line: 5, Column: 1},
					},
					{
						Name:      "Server",
						Kind:      codev0.SymbolKind_SYMBOL_KIND_STRUCT,
						Signature: "type Server struct",
						Location:  &codev0.Location{File: "main.go", Line: 10, Column: 1},
					},
				},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.ListSymbols(context.Background(), &gatewayv1.ListSymbolsRequest{File: "main.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != "" {
		t.Errorf("unexpected error field: %s", resp.Error)
	}
	if len(resp.Symbols) != 2 {
		t.Fatalf("expected 2 symbols, got %d", len(resp.Symbols))
	}
	if resp.Symbols[0].Name != "main" {
		t.Errorf("expected first symbol 'main', got %s", resp.Symbols[0].Name)
	}
	if resp.Symbols[0].Kind != "function" {
		t.Errorf("expected kind 'function', got %s", resp.Symbols[0].Kind)
	}
	if resp.Symbols[1].Name != "Server" {
		t.Errorf("expected second symbol 'Server', got %s", resp.Symbols[1].Name)
	}
}

func TestListSymbols_Error(t *testing.T) {
	mock := &mockCodeClient{
		listSymbolsFn: func(_ context.Context, _ *codev0.ListSymbolsRequest, _ ...grpc.CallOption) (*codev0.ListSymbolsResponse, error) {
			return &codev0.ListSymbolsResponse{
				Status: &codev0.ListSymbolsStatus{
					State:   codev0.ListSymbolsStatus_ERROR,
					Message: "LSP not ready",
				},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.ListSymbols(context.Background(), &gatewayv1.ListSymbolsRequest{File: "main.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != "LSP not ready" {
		t.Errorf("expected error 'LSP not ready', got %q", resp.Error)
	}
}

func TestGetDiagnostics(t *testing.T) {
	mock := &mockCodeClient{
		getDiagnosticsFn: func(_ context.Context, in *codev0.GetDiagnosticsRequest, _ ...grpc.CallOption) (*codev0.GetDiagnosticsResponse, error) {
			if in.File != "main.go" {
				t.Errorf("expected file main.go, got %s", in.File)
			}
			return &codev0.GetDiagnosticsResponse{
				Diagnostics: []*codev0.Diagnostic{
					{
						File:     "main.go",
						Line:     10,
						Column:   5,
						Message:  "unused variable 'x'",
						Severity: codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_WARNING,
						Source:   "gopls",
						Code:     "unusedvar",
					},
					{
						File:     "main.go",
						Line:     20,
						Column:   1,
						Message:  "missing return",
						Severity: codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_ERROR,
						Source:   "gopls",
					},
				},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.GetDiagnostics(context.Background(), &gatewayv1.GetDiagnosticsRequest{File: "main.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Diagnostics) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d", len(resp.Diagnostics))
	}
	d := resp.Diagnostics[0]
	if d.Message != "unused variable 'x'" {
		t.Errorf("expected message 'unused variable x', got %q", d.Message)
	}
	if d.Severity != "warning" {
		t.Errorf("expected severity 'warning', got %s", d.Severity)
	}
	if d.Source != "gopls" {
		t.Errorf("expected source 'gopls', got %s", d.Source)
	}
	d2 := resp.Diagnostics[1]
	if d2.Severity != "error" {
		t.Errorf("expected severity 'error', got %s", d2.Severity)
	}
}

func TestGoToDefinition(t *testing.T) {
	mock := &mockCodeClient{
		goToDefinitionFn: func(_ context.Context, in *codev0.GoToDefinitionRequest, _ ...grpc.CallOption) (*codev0.GoToDefinitionResponse, error) {
			if in.File != "main.go" || in.Line != 15 || in.Column != 10 {
				t.Errorf("unexpected request: file=%s line=%d col=%d", in.File, in.Line, in.Column)
			}
			return &codev0.GoToDefinitionResponse{
				Locations: []*codev0.Location{
					{File: "server.go", Line: 42, Column: 6, EndLine: 42, EndColumn: 12},
				},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.GoToDefinition(context.Background(), &gatewayv1.GoToDefinitionRequest{
		File: "main.go", Line: 15, Column: 10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Locations) != 1 {
		t.Fatalf("expected 1 location, got %d", len(resp.Locations))
	}
	loc := resp.Locations[0]
	if loc.File != "server.go" {
		t.Errorf("expected file server.go, got %s", loc.File)
	}
	if loc.Line != 42 {
		t.Errorf("expected line 42, got %d", loc.Line)
	}
}

func TestFindReferences(t *testing.T) {
	mock := &mockCodeClient{
		findReferencesFn: func(_ context.Context, in *codev0.FindReferencesRequest, _ ...grpc.CallOption) (*codev0.FindReferencesResponse, error) {
			return &codev0.FindReferencesResponse{
				Locations: []*codev0.Location{
					{File: "main.go", Line: 5, Column: 2},
					{File: "handler.go", Line: 12, Column: 8},
					{File: "test.go", Line: 30, Column: 4},
				},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.FindReferences(context.Background(), &gatewayv1.FindReferencesRequest{
		File: "main.go", Line: 5, Column: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Locations) != 3 {
		t.Fatalf("expected 3 references, got %d", len(resp.Locations))
	}
	if resp.Locations[1].File != "handler.go" {
		t.Errorf("expected second ref in handler.go, got %s", resp.Locations[1].File)
	}
}

func TestGetHoverInfo(t *testing.T) {
	mock := &mockCodeClient{
		getHoverInfoFn: func(_ context.Context, in *codev0.GetHoverInfoRequest, _ ...grpc.CallOption) (*codev0.GetHoverInfoResponse, error) {
			if in.File != "main.go" || in.Line != 10 || in.Column != 5 {
				t.Errorf("unexpected request: file=%s line=%d col=%d", in.File, in.Line, in.Column)
			}
			return &codev0.GetHoverInfoResponse{
				Content:  "func Serve(ctx context.Context) error",
				Language: "go",
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.GetHoverInfo(context.Background(), &gatewayv1.GetHoverInfoRequest{
		File: "main.go", Line: 10, Column: 5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "func Serve(ctx context.Context) error" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if resp.Language != "go" {
		t.Errorf("expected language 'go', got %s", resp.Language)
	}
}

func TestFix(t *testing.T) {
	mock := &mockCodeClient{
		fixFn: func(_ context.Context, in *codev0.FixRequest, _ ...grpc.CallOption) (*codev0.FixResponse, error) {
			if in.File != "main.go" {
				t.Errorf("expected file main.go, got %s", in.File)
			}
			return &codev0.FixResponse{
				Success: true,
				Content: "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }",
				Actions: []string{"goimports", "gofmt"},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.Fix(context.Background(), &gatewayv1.FixRequest{Path: "main.go"})
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
}

func TestApplyEdit(t *testing.T) {
	mock := &mockCodeClient{
		applyEditFn: func(_ context.Context, in *codev0.ApplyEditRequest, _ ...grpc.CallOption) (*codev0.ApplyEditResponse, error) {
			if in.File != "main.go" || in.Find != "old code" || in.Replace != "new code" {
				t.Errorf("unexpected request: file=%s find=%q replace=%q", in.File, in.Find, in.Replace)
			}
			return &codev0.ApplyEditResponse{
				Success:    true,
				Content:    "package main\n\nnew code\n",
				Strategy:   "exact",
				FixActions: []string{"goimports"},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.ApplyEdit(context.Background(), &gatewayv1.ApplyEditRequest{
		File: "main.go", Find: "old code", Replace: "new code", AutoFix: true,
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
	if len(resp.FixActions) != 1 || resp.FixActions[0] != "goimports" {
		t.Errorf("unexpected fix actions: %v", resp.FixActions)
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
	s := newTestServerWithWorkDir(&mockCodeClient{}, dir)

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

func TestRenameSymbol(t *testing.T) {
	mock := &mockCodeClient{
		renameSymbolFn: func(_ context.Context, in *codev0.RenameSymbolRequest, _ ...grpc.CallOption) (*codev0.RenameSymbolResponse, error) {
			if in.NewName != "NewName" {
				t.Errorf("expected NewName, got %s", in.NewName)
			}
			return &codev0.RenameSymbolResponse{
				Success: true,
				Edits: []*codev0.TextEdit{
					{File: "main.go", StartLine: 5, StartColumn: 6, EndLine: 5, EndColumn: 13, NewText: "NewName"},
				},
				Files: []string{"main.go"},
			}, nil
		},
	}
	s := newTestServer(mock)
	resp, err := s.RenameSymbol(context.Background(), &gatewayv1.RenameSymbolRequest{
		File: "main.go", Line: 5, Column: 6, NewName: "NewName",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success {
		t.Errorf("expected Success=true, error=%s", resp.Error)
	}
	if len(resp.Edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(resp.Edits))
	}
	if resp.Edits[0].NewText != "NewName" {
		t.Errorf("expected NewText 'NewName', got %s", resp.Edits[0].NewText)
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
	callCount := 0
	mock := &mockCodeClient{
		applyEditFn: func(_ context.Context, in *codev0.ApplyEditRequest, _ ...grpc.CallOption) (*codev0.ApplyEditResponse, error) {
			callCount++
			return &codev0.ApplyEditResponse{
				Success:  true,
				Content:  "updated content",
				Strategy: "exact",
			}, nil
		},
	}
	s := newTestServer(mock)
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
	if callCount != 2 {
		t.Errorf("expected 2 applyEdit calls, got %d", callCount)
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

func TestDiagSeverityToString(t *testing.T) {
	tests := []struct {
		sev  codev0.DiagnosticSeverity
		want string
	}{
		{codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_ERROR, "error"},
		{codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_WARNING, "warning"},
		{codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_INFORMATION, "information"},
		{codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_HINT, "hint"},
		{codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_UNKNOWN, "unknown"},
	}
	for _, tt := range tests {
		got := diagSeverityToString(tt.sev)
		if got != tt.want {
			t.Errorf("diagSeverityToString(%v) = %q, want %q", tt.sev, got, tt.want)
		}
	}
}
