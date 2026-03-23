package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestMCPServer_ListTools_IncludesServiceTools(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(ctx, "test-version")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	tools := server.ListTools()
	expectedServiceTools := []string{"describe", "read_file", "write_file", "list_symbols", "build", "run_checks", "stop"}
	for _, name := range expectedServiceTools {
		found := false
		for _, tool := range tools {
			if tool.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected per-service tool %q not found", name)
		}
	}
}

func TestServiceTools_WithoutWorkspace(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(ctx, "test-version")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	// NewServer with no workspace leaves server.workspace == nil when not in a workspace dir

	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"describe","arguments":{"module":"m","service":"s"}}`),
	}

	resp := server.handleRequest(ctx, req)
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result CallToolResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if !result.IsError || len(result.Content) == 0 {
		t.Logf("result: %+v", result)
		t.Error("expected describe without workspace to return error content")
	}
}

func TestServiceTools_Describe_MissingArgs(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(ctx, "test-version")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"describe","arguments":{}}`),
	}
	resp := server.handleRequest(ctx, req)
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result CallToolResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if !result.IsError {
		t.Error("expected describe with no module/service to return error")
	}
}
