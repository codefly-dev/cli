package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These lock the behavior of the introspection tools now that they delegate to
// the control plane (the first Phase-3 adapter): the tool output must still be
// produced correctly from a real workspace on disk.

func writeMCPWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                            "name: demo\nlayout: modules\nmodules:\n    - name: backend\n",
		"modules/backend/module.codefly.yaml":               "kind: module\nname: backend\nservices:\n    - name: api\n",
		"modules/backend/services/api/service.codefly.yaml": "kind: service\nname: api\nversion: 0.0.0\nmodule: backend\nagent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func callTool(t *testing.T, server *Server, ctx context.Context, name, argsJSON string) CallToolResult {
	t.Helper()
	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"` + name + `","arguments":` + argsJSON + `}`),
	}
	resp := server.handleRequest(ctx, req)
	if resp.Error != nil {
		t.Fatalf("protocol error: %s", resp.Error.Message)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	var result CallToolResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("tool returned no content")
	}
	return result
}

func TestListServicesToolViaControlPlane(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	text := callTool(t, server, ctx, "list_services", `{}`).Content[0].Text
	for _, want := range []string{`"name": "api"`, `"module": "backend"`, `"agent": "go-grpc"`, `"version": "0.0.0"`} {
		if !strings.Contains(text, want) {
			t.Errorf("list_services output missing %q:\n%s", want, text)
		}
	}
}

func TestWorkspaceInfoToolViaControlPlane(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	text := callTool(t, server, ctx, "workspace_info", `{}`).Content[0].Text
	for _, want := range []string{`"name": "demo"`, `"backend"`, `"service": "api"`} {
		if !strings.Contains(text, want) {
			t.Errorf("workspace_info output missing %q:\n%s", want, text)
		}
	}
}

func TestListModulesToolViaControlPlane(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	text := callTool(t, server, ctx, "list_modules", `{}`).Content[0].Text
	if !strings.Contains(text, `"name": "backend"`) {
		t.Errorf("list_modules output missing backend:\n%s", text)
	}
}
