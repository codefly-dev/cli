package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

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
