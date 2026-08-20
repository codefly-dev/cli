package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHowToToolRegistered(t *testing.T) {
	server, err := NewServer(context.Background(), "test-version")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	found := false
	for _, tool := range server.ListTools() {
		if tool.Name == "how_to" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("how_to tool not registered")
	}
}

func TestHowToListsAndFetches(t *testing.T) {
	server, err := NewServer(context.Background(), "test-version")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// No topic: lists available runbooks.
	list := callHowTo(t, server, `{}`)
	if !strings.Contains(list, "bump-go-version") {
		t.Errorf("list should mention bump-go-version, got:\n%s", list)
	}

	// A specific topic: returns the full runbook.
	full := callHowTo(t, server, `{"topic":"bump-go-version"}`)
	if !strings.Contains(full, "# Runbook: Bump the Go version") {
		t.Errorf("topic fetch should return the runbook body, got:\n%s", full)
	}

	// Unknown topic: graceful message, not an error.
	unknown := callHowTo(t, server, `{"topic":"nope"}`)
	if !strings.Contains(unknown, "Unknown runbook") {
		t.Errorf("unknown topic should be reported, got:\n%s", unknown)
	}
}

func callHowTo(t *testing.T, server *Server, arguments string) string {
	t.Helper()
	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"how_to","arguments":` + arguments + `}`),
	}
	resp := server.handleRequest(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result CallToolResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}
	return result.Content[0].Text
}
