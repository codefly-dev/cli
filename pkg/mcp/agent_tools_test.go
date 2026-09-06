package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
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

func writeLocalAgentCache(t *testing.T, home, publisher string, installs ...string) {
	t.Helper()
	dir := filepath.Join(home, "agents", "services", publisher)
	for _, install := range installs {
		if err := os.MkdirAll(filepath.Join(dir, install), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListAgentsReadsLocalCacheWithSemverOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEFLY_HOME", home)
	writeLocalAgentCache(t, home, "codefly.dev", "nextjs__0.0.9", "nextjs__0.0.10", "nextjs__garbage")
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

// TestListAgentsMergesPinnedAndInstalledSameAgent locks in the fix for a real
// merge failure: a workspace pin's agent.kind ("runtime::service", the only
// spelling used by every service.codefly.yaml in this codebase) and the local
// cache's canonical kind ("codefly:service") used to be compared literally,
// so the same agent (same publisher/name/version, both pinned AND installed)
// showed up as two separate rows instead of one merged row.
func TestListAgentsMergesPinnedAndInstalledSameAgent(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	home := t.TempDir()
	t.Setenv("CODEFLY_HOME", home)
	// writeMCPWorkspace pins codefly.ai/go-grpc:0.0.16 — install the exact
	// same agent in the local cache so the two sources describe one agent.
	writeLocalAgentCache(t, home, "codefly.ai", "go-grpc__0.0.16")

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
		t.Fatalf("expected the pinned and installed rows to merge into 1 entry, got %d:\n%s", len(entries), text)
	}

	entry := entries[0]
	if !reflect.DeepEqual(entry.PinnedVersions, []string{"0.0.16"}) {
		t.Errorf("pinned_versions = %v, want [0.0.16]", entry.PinnedVersions)
	}
	if !reflect.DeepEqual(entry.PinnedBy, []string{"backend/api"}) {
		t.Errorf("pinned_by = %v, want [backend/api]", entry.PinnedBy)
	}
	if !reflect.DeepEqual(entry.InstalledVersions, []string{"0.0.16"}) {
		t.Errorf("installed_versions = %v, want [0.0.16]", entry.InstalledVersions)
	}
	if entry.Kind != string(resources.ServiceAgent) {
		t.Errorf("kind = %q, want the canonical %q so the merged row also matches kind=service filters", entry.Kind, resources.ServiceAgent)
	}
}

// TestListAgentsKindFilterIncludesPinnedOnlyAgent locks in the other half of
// the same fix: filtering by the canonical kind must still find an agent
// that is pinned but never installed locally, even though its raw
// agent.kind ("runtime::service") is not itself one of the canonical
// resources.AgentKind values the filter argument maps to.
func TestListAgentsKindFilterIncludesPinnedOnlyAgent(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	t.Setenv("CODEFLY_HOME", t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "list_agents", `{"kind":"service"}`).Content[0].Text
	var entries []agentListEntry
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		t.Fatalf("parse list_agents output: %v\n%s", err, text)
	}
	if len(entries) != 1 || entries[0].Name != "go-grpc" {
		t.Fatalf("expected the pinned-only go-grpc agent under kind=service, got:\n%s", text)
	}
}

// TestSortVersionsDescendingHandlesNonStrictSemver locks in the fix for a
// non-transitive comparator: the old "semver-compare if both parse, else
// string-compare" rule changed behavior per pair, so a free-form pinned
// version like "1.10" (valid YAML, invalid strict semver — it has no patch
// component) could sort below an older version. blang/semver rejects "1.10"
// outright; Masterminds/semver accepts it as 1.10.0, matching how real
// pinned versions actually look in service.codefly.yaml.
func TestSortVersionsDescendingHandlesNonStrictSemver(t *testing.T) {
	versions := []string{"1.9.0", "2.0.0", "1.10"}
	sortVersionsDescending(versions)
	want := []string{"2.0.0", "1.10", "1.9.0"}
	if !reflect.DeepEqual(versions, want) {
		t.Errorf("sortVersionsDescending(...) = %v, want %v", versions, want)
	}
}

func TestListAgentsKindFilter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEFLY_HOME", home)
	writeLocalAgentCache(t, home, "codefly.dev", "nextjs__0.0.9")
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

// TestAgentInfoRejectsVersionPathTraversal locks in the fix for a path
// traversal: resources.Agent.Path() builds the on-disk binary path as
// "<publisher>/<name>__<version>" by string concatenation before path.Join,
// so an unvalidated version can walk out of the agent cache directory
// entirely. Name and Publisher pass length/charset validation at ParseAgent
// (so this specific payload isn't rejected there — this test asserts the
// version-specific guard actually fires, not just "some" rejection), and
// the version has no such constraint anywhere else in the stack.
func TestAgentInfoRejectsVersionPathTraversal(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	// codefly.dev (11 chars) and go-grpc (7 chars) both clear ParseAgent's
	// min-length validation, so this reference reaches the version check.
	result := callTool(t, server, ctx, "agent_info", `{"agent":"codefly.dev/go-grpc:../../../../../../etc/passwd"}`)
	if !result.IsError {
		t.Errorf("expected agent_info to reject a path-traversal version, got: %+v", result)
	}
}

// TestAgentInfoRejectsUnknownKind locks in the fix for agent_info being
// hardcoded to resources.ServiceAgent: list_agents advertises 6 other kinds
// (job, application, module, toolbox, provider, solution) via the same
// "kind" argument vocabulary, so agent_info must recognize and validate it
// too instead of silently ignoring it.
func TestAgentInfoRejectsUnknownKind(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	result := callTool(t, server, ctx, "agent_info", `{"agent":"codefly.dev/go-grpc:0.0.1","kind":"bogus"}`)
	if !result.IsError {
		t.Errorf("expected agent_info to reject an unknown kind, got: %+v", result)
	}
}

// TestAgentInfoUsesRequestedKind proves the "kind" argument actually changes
// which resources.AgentKindRegistration agent_info resolves against, rather
// than being accepted and ignored. Provider is the differential signal:
// unlike ServiceAgent, ProviderAgent has no GitHub auto-download configured,
// so a provider lookup for a not-installed agent fails fast and offline
// with a distinct error; if the kind argument were being dropped (falling
// back to ServiceAgent, which does have GitHub auto-download configured),
// this would instead attempt a real network call.
func TestAgentInfoUsesRequestedKind(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CODEFLY_HOME", t.TempDir())
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	result := callTool(t, server, ctx, "agent_info", `{"agent":"codefly.dev/vault:0.0.1","kind":"provider"}`)
	if !result.IsError {
		t.Fatalf("expected agent_info to fail for a not-installed provider agent, got: %+v", result)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "no verified remote resolution") {
		t.Errorf("expected the provider-specific offline failure, got: %s", text)
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

	// Call twice: agent_info now tears down its own agent connection after
	// each call (see the cacheKey/ClearAgent comment in agentInfo) instead
	// of keeping it cached under a shared key for the server's lifetime, so
	// a second call must independently reload and succeed rather than reuse
	// a connection that no longer exists.
	for i := 0; i < 2; i++ {
		text := callTool(t, server, ctx, "agent_info", `{"agent":"postgres"}`).Content[0].Text
		var info map[string]any
		if err := json.Unmarshal([]byte(text), &info); err != nil {
			t.Fatalf("call %d: parse agent_info output: %v\n%s", i, err, text)
		}
		if caps, _ := info["capabilities"].([]any); len(caps) == 0 {
			t.Errorf("call %d: expected non-empty capabilities:\n%s", i, text)
		}
		if backends, _ := info["supported_backends"].([]any); len(backends) == 0 {
			t.Errorf("call %d: expected non-empty supported_backends:\n%s", i, text)
		}
	}
}
