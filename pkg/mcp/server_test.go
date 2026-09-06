package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSyncBuffer is a bytes.Buffer safe for one writer goroutine and one
// polling reader goroutine, needed because ServeIO now dispatches concurrent
// request handlers that can write to the same output stream.
type testSyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *testSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *testSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func writeLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
}

// TestServeIOHandlesRequestsConcurrentlySoASlowToolDoesNotBlockOthers proves
// ServeIO no longer processes JSON-RPC requests one at a time: a tool call
// blocked mid-handler must not prevent a second, independent request from
// being read and answered. Before this fix, run_service's blocking wait
// (default up to 300s) froze the entire server — including flow_status and
// stop_flow, which exist specifically to be usable while a run is in
// progress.
func TestServeIOHandlesRequestsConcurrentlySoASlowToolDoesNotBlockOthers(t *testing.T) {
	server, err := NewServer(context.Background(), "test-version")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	entered := make(chan struct{})
	release := make(chan struct{})
	if err := server.RegisterTool(Tool{Name: "blocking_tool"}, func(_ context.Context, _ map[string]string) ([]Content, error) {
		close(entered)
		<-release
		return []Content{TextContent("slow done")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterTool(Tool{Name: "fast_tool"}, func(_ context.Context, _ map[string]string) ([]Content, error) {
		return []Content{TextContent("fast done")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	reader, writer := io.Pipe()
	var out testSyncBuffer
	done := make(chan error, 1)
	go func() { done <- server.ServeIO(context.Background(), reader, &out) }()

	writeLine(t, writer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blocking_tool","arguments":{}}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking_tool handler was never entered")
	}

	writeLine(t, writer, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fast_tool","arguments":{}}}`)

	deadline := time.After(2 * time.Second)
	for !strings.Contains(out.String(), `"id":2`) {
		select {
		case <-deadline:
			t.Fatal("fast_tool's response never arrived while blocking_tool was still blocked — requests are still serialized")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if strings.Contains(out.String(), "slow done") {
		t.Fatal("blocking_tool's response arrived before it was released")
	}

	close(release)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeIO returned error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeIO did not return after both requests completed")
	}
	if !strings.Contains(out.String(), "slow done") {
		t.Fatal("blocking_tool's response never arrived after it was released")
	}
}

func TestMCPServer_Initialize(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(ctx, "test-version")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Create initialize request
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"1.0"}}`),
	}

	reqBytes, _ := json.Marshal(req)
	input := bytes.NewReader(append(reqBytes, '\n'))
	output := &bytes.Buffer{}

	// Run server with single request
	go func() {
		_ = server.ServeIO(ctx, input, output)
	}()

	// Wait for response
	// Note: In a real test, we'd use proper synchronization
}

func TestMCPServerServeIOHonorsCancellationWhileInputBlocks(t *testing.T) {
	server, err := NewServer(context.Background(), "test-version")
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeIO(ctx, reader, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ServeIO error = %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeIO did not stop after context cancellation")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestMCPServerParseErrorPropagatesWriteFailure(t *testing.T) {
	server, err := NewServer(context.Background(), "test-version")
	if err != nil {
		t.Fatal(err)
	}
	err = server.ServeIO(context.Background(), strings.NewReader("not-json\n"), failingWriter{})
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("ServeIO error = %v", err)
	}
}

func TestMCPServer_NotificationsDoNotWriteResponses(t *testing.T) {
	server, err := NewServer(context.Background(), "test-version")
	if err != nil {
		t.Fatal(err)
	}

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/unknown"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := server.ServeIO(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected responses only for the two requests, got %d lines: %q", len(lines), output.String())
	}
	if strings.Contains(output.String(), "null\n") || strings.Contains(output.String(), "notifications/unknown") {
		t.Fatalf("notification leaked a protocol response: %q", output.String())
	}
}

func TestMCPServer_ListTools(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(ctx, "test-version")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	tools := server.ListTools()
	if len(tools) == 0 {
		t.Error("expected at least one tool")
	}

	// Check for expected tools
	expectedTools := []string{"workspace_info", "list_modules", "list_services", "service_info", "list_agents"}
	for _, expected := range expectedTools {
		found := false
		for _, tool := range tools {
			if tool.Name == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected tool %s not found", expected)
		}
	}
}

func TestMCPToolsAreCallableThroughInProcessToolbox(t *testing.T) {
	server, err := NewServer(context.Background(), "test-version")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	server.RegisterTool(Tool{Name: "in_process_echo"}, func(_ context.Context, arguments map[string]string) ([]Content, error) {
		return []Content{TextContent(arguments["text"])}, nil
	})
	content, err := server.Toolbox().Call(context.Background(), "in_process_echo", map[string]string{"text": "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 1 || content[0].Text != "shared" {
		t.Fatalf("toolbox content = %+v", content)
	}
}

func TestMCPServer_HandleRequest(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(ctx, "test-version")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	tests := []struct {
		name       string
		method     string
		params     string
		wantError  bool
		checkField string
	}{
		{
			name:   "initialize",
			method: "initialize",
			params: `{"protocolVersion":"2024-11-05"}`,
		},
		{
			name:   "list_tools",
			method: "tools/list",
			params: `{}`,
		},
		{
			name:   "ping",
			method: "ping",
			params: `{}`,
		},
		{
			name:      "unknown_method",
			method:    "unknown/method",
			params:    `{}`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &JSONRPCRequest{
				JSONRPC: "2.0",
				ID:      1,
				Method:  tt.method,
				Params:  json.RawMessage(tt.params),
			}

			resp := server.handleRequest(ctx, req)
			if resp == nil && tt.method == "initialized" {
				// initialized doesn't return a response
				return
			}

			if tt.wantError {
				if resp.Error == nil {
					t.Error("expected error response")
				}
			} else {
				if resp.Error != nil {
					t.Errorf("unexpected error: %s", resp.Error.Message)
				}
			}
		})
	}
}

func TestMCPServer_CallTool(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(ctx, "test-version")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Test list_agents tool (doesn't require workspace)
	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"list_agents","arguments":{}}`),
	}

	resp := server.handleRequest(ctx, req)
	if resp.Error != nil {
		t.Errorf("unexpected error: %s", resp.Error.Message)
	}

	// Parse result
	resultBytes, _ := json.Marshal(resp.Result)
	var result CallToolResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if len(result.Content) == 0 {
		t.Error("expected content in result")
	}

	// Check that result contains agent information
	content := result.Content[0].Text
	if !strings.Contains(content, "go-grpc") {
		t.Error("expected go-grpc agent in result")
	}
}
