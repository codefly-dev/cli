package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// These lock in the fix for exact-match resource lookup: module, service, and
// endpoints resources are documented as URI templates but must resolve for
// any concrete URI matching the template, and resources/list must enumerate
// them concretely for the loaded workspace.

func readResource(t *testing.T, server *Server, ctx context.Context, uri string) *JSONRPCResponse {
	t.Helper()
	params, err := json.Marshal(ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatal(err)
	}
	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "resources/read",
		Params:  params,
	}
	return server.handleRequest(ctx, req)
}

func TestResourcesListEnumeratesConcreteResources(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := server.handleRequest(ctx, &JSONRPCRequest{JSONRPC: "2.0", ID: 1, Method: "resources/list"})
	if resp.Error != nil {
		t.Fatalf("protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result ListResourcesResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatal(err)
	}

	got := make(map[string]bool)
	for _, r := range result.Resources {
		got[r.URI] = true
	}
	want := []string{
		"codefly://workspace",
		"codefly://module/backend",
		"codefly://service/backend/api",
		"codefly://endpoints/backend/api",
	}
	for _, uri := range want {
		if !got[uri] {
			t.Errorf("resources/list missing %q, got %v", uri, result.Resources)
		}
	}
	if len(result.Resources) != len(want) {
		t.Errorf("resources/list returned %d resources, want %d: %v", len(result.Resources), len(want), result.Resources)
	}
}

func TestResourceTemplatesList(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := server.handleRequest(ctx, &JSONRPCRequest{JSONRPC: "2.0", ID: 1, Method: "resources/templates/list"})
	if resp.Error != nil {
		t.Fatalf("protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result ListResourceTemplatesResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatal(err)
	}

	if len(result.ResourceTemplates) != 3 {
		t.Fatalf("expected 3 resource templates, got %d: %v", len(result.ResourceTemplates), result.ResourceTemplates)
	}
	got := make(map[string]bool)
	for _, tmpl := range result.ResourceTemplates {
		got[tmpl.URITemplate] = true
	}
	for _, want := range []string{
		"codefly://module/{module}",
		"codefly://service/{module}/{service}",
		"codefly://endpoints/{module}/{service}",
	} {
		if !got[want] {
			t.Errorf("resources/templates/list missing %q, got %v", want, result.ResourceTemplates)
		}
	}
}

func TestReadModuleResource(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := readResource(t, server, ctx, "codefly://module/backend")
	if resp.Error != nil {
		t.Fatalf("protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result ReadResourceResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(result.Contents))
	}
	if result.Contents[0].MimeType != "application/x-yaml" {
		t.Errorf("expected application/x-yaml, got %q", result.Contents[0].MimeType)
	}
	if !strings.Contains(result.Contents[0].Text, "name: backend") {
		t.Errorf("expected text to contain %q, got %q", "name: backend", result.Contents[0].Text)
	}
}

func TestReadServiceResource(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := readResource(t, server, ctx, "codefly://service/backend/api")
	if resp.Error != nil {
		t.Fatalf("protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result ReadResourceResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(result.Contents))
	}
	if !strings.Contains(result.Contents[0].Text, "name: api") {
		t.Errorf("expected text to contain %q, got %q", "name: api", result.Contents[0].Text)
	}
}

func TestReadEndpointsResource(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := readResource(t, server, ctx, "codefly://endpoints/backend/api")
	if resp.Error != nil {
		t.Fatalf("protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result ReadResourceResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(result.Contents))
	}
	if result.Contents[0].MimeType != "application/json" {
		t.Errorf("expected application/json, got %q", result.Contents[0].MimeType)
	}
	var endpoints []map[string]any
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &endpoints); err != nil {
		t.Fatalf("expected valid JSON array, got %q: %v", result.Contents[0].Text, err)
	}
	if len(endpoints) != 0 {
		t.Errorf("expected empty endpoints array, got %v", endpoints)
	}
}

func TestReadUnknownModuleResource(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := readResource(t, server, ctx, "codefly://module/nope")
	if resp.Error == nil {
		t.Fatal("expected error for unknown module")
	}
	if resp.Error.Code != ResourceNotFound {
		t.Errorf("expected code %d, got %d", ResourceNotFound, resp.Error.Code)
	}
}

func TestReadServiceResourceWrongArity(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := readResource(t, server, ctx, "codefly://service/backend")
	if resp.Error == nil {
		t.Fatal("expected error for wrong-arity service URI")
	}
	if resp.Error.Code != ResourceNotFound {
		t.Errorf("expected code %d, got %d", ResourceNotFound, resp.Error.Code)
	}
}

func TestReadNonCodeflyURI(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := readResource(t, server, ctx, "file:///etc/passwd")
	if resp.Error == nil {
		t.Fatal("expected error for non-codefly URI")
	}
	if resp.Error.Code != ResourceNotFound {
		t.Errorf("expected code %d, got %d", ResourceNotFound, resp.Error.Code)
	}
}

func TestResourcesWithoutWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := server.handleRequest(ctx, &JSONRPCRequest{JSONRPC: "2.0", ID: 1, Method: "resources/list"})
	if resp.Error != nil {
		t.Fatalf("protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result ListResourcesResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 1 || result.Resources[0].URI != "codefly://workspace" {
		t.Errorf("expected only codefly://workspace, got %v", result.Resources)
	}

	readResp := readResource(t, server, ctx, "codefly://module/x")
	if readResp.Error == nil {
		t.Fatal("expected error reading module without a workspace")
	}
	if readResp.Error.Code != ResourceNotFound {
		t.Errorf("expected code %d, got %d", ResourceNotFound, readResp.Error.Code)
	}
}
