package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestListAgentsReflectsWorkspacePins(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	t.Setenv("CODEFLY_HOME", t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "list_agents", `{}`).Content[0].Text
	var entries []agentListEntry
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		t.Fatalf("parse list_agents output: %v\n%s", err, text)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 agent, got %d:\n%s", len(entries), text)
	}

	entry := entries[0]
	if entry.Name != "go-grpc" || entry.Publisher != "codefly.ai" {
		t.Errorf("unexpected agent identity: %+v", entry)
	}
	if !reflect.DeepEqual(entry.PinnedVersions, []string{"0.0.16"}) {
		t.Errorf("pinned_versions = %v, want [0.0.16]", entry.PinnedVersions)
	}
	if !reflect.DeepEqual(entry.PinnedBy, []string{"backend/api"}) {
		t.Errorf("pinned_by = %v, want [backend/api]", entry.PinnedBy)
	}
	if len(entry.InstalledVersions) != 0 {
		t.Errorf("installed_versions = %v, want empty", entry.InstalledVersions)
	}
}

func writeLocalAgentCache(t *testing.T, home string, installs ...string) {
	t.Helper()
	dir := filepath.Join(home, "agents", "services", "codefly.dev")
	for _, install := range installs {
		if err := os.MkdirAll(filepath.Join(dir, install), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListAgentsReadsLocalCacheWithSemverOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEFLY_HOME", home)
	writeLocalAgentCache(t, home, "nextjs__0.0.9", "nextjs__0.0.10", "nextjs__garbage")
	t.Chdir(t.TempDir())

	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "list_agents", `{}`).Content[0].Text
	var entries []agentListEntry
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		t.Fatalf("parse list_agents output: %v\n%s", err, text)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 agent, got %d:\n%s", len(entries), text)
	}

	entry := entries[0]
	if entry.Name != "nextjs" {
		t.Errorf("name = %q, want nextjs", entry.Name)
	}
	if !reflect.DeepEqual(entry.InstalledVersions, []string{"0.0.10", "0.0.9"}) {
		t.Errorf("installed_versions = %v, want [0.0.10 0.0.9]", entry.InstalledVersions)
	}
	if len(entry.PinnedBy) != 0 {
		t.Errorf("pinned_by = %v, want empty", entry.PinnedBy)
	}
}

func TestListAgentsKindFilter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEFLY_HOME", home)
	writeLocalAgentCache(t, home, "nextjs__0.0.9")
	t.Chdir(t.TempDir())

	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "list_agents", `{"kind":"toolbox"}`).Content[0].Text
	var entries []agentListEntry
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		t.Fatalf("parse list_agents output: %v\n%s", err, text)
	}
	if len(entries) != 0 {
		t.Errorf("expected no agents for kind=toolbox, got %v", entries)
	}
}

func TestDescribeHasNoHardcodedLanguage(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "describe", `{"module":"backend","service":"api"}`).Content[0].Text
	if strings.Contains(text, `"language"`) {
		t.Errorf("describe output still contains a hardcoded language:\n%s", text)
	}
	if !strings.Contains(text, `"publisher": "codefly.ai"`) {
		t.Errorf("describe output missing agent publisher:\n%s", text)
	}
}

func TestAgentInfoRejectsUnsafeReference(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	result := callTool(t, server, ctx, "agent_info", `{"agent":"../x"}`)
	if !result.IsError {
		t.Errorf("expected agent_info to reject an unsafe agent reference, got: %+v", result)
	}
}

func TestAgentInfoLoadsRealAgent(t *testing.T) {
	if os.Getenv("CODEFLY_MCP_AGENT_QUALIFY") != "1" {
		t.Skip("set CODEFLY_MCP_AGENT_QUALIFY=1 to run against a real installed agent")
	}
	t.Chdir(t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "agent_info", `{"agent":"postgres"}`).Content[0].Text
	var info map[string]any
	if err := json.Unmarshal([]byte(text), &info); err != nil {
		t.Fatalf("parse agent_info output: %v\n%s", err, text)
	}
	if caps, _ := info["capabilities"].([]any); len(caps) == 0 {
		t.Errorf("expected non-empty capabilities:\n%s", text)
	}
	if backends, _ := info["supported_backends"].([]any); len(backends) == 0 {
		t.Errorf("expected non-empty supported_backends:\n%s", text)
	}
}
