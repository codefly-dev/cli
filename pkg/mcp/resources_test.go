package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These lock in the fix for exact-match resource lookup: module, service, and
// endpoints resources are documented as URI templates but must resolve for
// any concrete URI matching the template, and resources/list must enumerate
// them concretely for the loaded workspace.

// writeMCPWorkspaceWithPinnedDependency builds a two-module workspace where
// "platform" is a normal local module and "saas" is an identity-only
// reference (source+version, no local checkout) that resolves to
// ResolutionPinned and can never load locally. This is the shape a
// composition-root workspace takes before its pinned dependencies are
// pulled, and it must not take the rest of the workspace down with it.
func writeMCPWorkspaceWithPinnedDependency(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                             "name: solution\nlayout: modules\nmodules:\n    - name: platform\n    - name: saas\n      source: acme/host\n      version: \">=0.0.44\"\n",
		"modules/platform/module.codefly.yaml":               "kind: module\nname: platform\nservices:\n    - name: api\n",
		"modules/platform/services/api/service.codefly.yaml": "kind: service\nname: api\nversion: 0.0.0\nmodule: platform\nagent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
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

// A workspace composing an unpulled pinned dependency alongside a normal
// local module must still enumerate the local module's resources: one
// unloadable reference must not blank out the rest of the workspace.
func TestResourcesListSkipsOnlyTheUnloadableModule(t *testing.T) {
	t.Chdir(writeMCPWorkspaceWithPinnedDependency(t))
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
	for _, uri := range []string{
		"codefly://workspace",
		"codefly://module/platform",
		"codefly://service/platform/api",
		"codefly://endpoints/platform/api",
	} {
		if !got[uri] {
			t.Errorf("resources/list missing %q (an unrelated pinned module must not blank out the rest of the workspace), got %v", uri, result.Resources)
		}
	}
	if got["codefly://module/saas"] {
		t.Errorf("resources/list should not include the unloadable pinned module 'saas', got %v", result.Resources)
	}
}

// A module that is declared in the workspace manifest but fails to load for
// a reason other than "not declared" (here: it's a pinned dependency that
// hasn't been pulled locally) must not be reported as ResourceNotFound. That
// code claims the resource does not exist, which is false and misleads a
// client into thinking the dependency was never declared.
func TestReadDeclaredButUnloadableModuleIsNotReportedNotFound(t *testing.T) {
	t.Chdir(writeMCPWorkspaceWithPinnedDependency(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	resp := readResource(t, server, ctx, "codefly://module/saas")
	if resp.Error == nil {
		t.Fatal("expected an error reading a pinned module that can't be loaded locally")
	}
	if resp.Error.Code == ResourceNotFound {
		t.Errorf("module 'saas' is declared in the workspace manifest; reporting ResourceNotFound (-32002) falsely claims it was never declared, got message %q", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "saas") {
		t.Errorf("expected error message to name the module, got %q", resp.Error.Message)
	}
}
