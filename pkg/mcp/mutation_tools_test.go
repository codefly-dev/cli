package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/blang/semver"
	"github.com/codefly-dev/core/resources"
)

func TestAddServiceRejectsUnsafeName(t *testing.T) {
	root := writeMCPWorkspace(t)
	t.Chdir(root)
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "add_service", `{"module":"backend","name":"../evil","agent":"go-grpc"}`).Content[0].Text
	if !strings.Contains(text, "invalid service name") {
		t.Fatalf("expected invalid service name error, got: %s", text)
	}
	if _, err := os.Stat(filepath.Join(root, "modules", "backend", "evil")); !os.IsNotExist(err) {
		t.Fatalf("expected no directory to be created outside services/, stat err: %v", err)
	}
}

func TestAddServiceRefusesExistingService(t *testing.T) {
	root := writeMCPWorkspace(t)
	t.Chdir(root)
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	yamlPath := filepath.Join(root, "modules", "backend", "services", "api", "service.codefly.yaml")
	before, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "add_service", `{"module":"backend","name":"api","agent":"go-grpc"}`).Content[0].Text
	if !strings.Contains(text, "already exists") {
		t.Fatalf("expected already exists error, got: %s", text)
	}

	after, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("existing service.codefly.yaml was modified:\nbefore: %s\nafter: %s", before, after)
	}
}

func TestAddServiceRejectsUntrustedPublisher(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "add_service", `{"module":"backend","name":"store","agent":"other-org/go-grpc"}`).Content[0].Text
	if !strings.Contains(text, "not allowed") {
		t.Fatalf("expected publisher rejection, got: %s", text)
	}
	if _, err := os.Stat(filepath.Join("modules", "backend", "services", "store")); !os.IsNotExist(err) {
		t.Fatalf("expected no service directory to be created for a rejected publisher, stat err: %v", err)
	}
}

func TestAddServiceRejectsUnsafeAgentPublisherChars(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "add_service", `{"module":"backend","name":"store","agent":"weird$(rm)/go-grpc"}`).Content[0].Text
	if !strings.Contains(text, "invalid agent") {
		t.Fatalf("expected invalid agent error, got: %s", text)
	}
}

func TestIsSafeAgentNameRejectsPathTraversalSegments(t *testing.T) {
	for _, unsafe := range []string{".", ".."} {
		if isSafeAgentName(unsafe) {
			t.Errorf("isSafeAgentName(%q) = true, want false (path-traversal segment)", unsafe)
		}
	}
	for _, safe := range []string{"go-grpc", "v1.2.3", "postgres_agent"} {
		if !isSafeAgentName(safe) {
			t.Errorf("isSafeAgentName(%q) = false, want true", safe)
		}
	}
}

func TestTruncateKeepsValidUTF8AtByteBoundary(t *testing.T) {
	// "€" is a 3-byte UTF-8 sequence (E2 82 AC). Placed right after 3999 'a's,
	// its first byte lands at index 3999 and its second byte at index 4000 —
	// truncating at n=4000 with a raw byte slice would cut it in half.
	s := strings.Repeat("a", 3999) + "€" + strings.Repeat("b", 10)

	got := truncate(s, 4000)
	prefix := strings.TrimSuffix(got, "\n… (truncated)")

	if !utf8.ValidString(prefix) {
		t.Fatalf("truncated prefix is not valid UTF-8: %q", prefix)
	}
	if !strings.HasPrefix(s, prefix) {
		t.Fatalf("truncated prefix %q is not a genuine prefix of the original string", prefix)
	}
	if len(prefix) >= 4000 {
		t.Fatalf("expected the cut to move back before the split rune, got prefix length %d", len(prefix))
	}
}

func TestAddServiceUnknownModule(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	result := callTool(t, server, ctx, "add_service", `{"module":"nope","name":"x","agent":"go-grpc"}`)
	if !result.IsError {
		t.Fatalf("expected an error result, got: %+v", result)
	}
	if !strings.Contains(result.Content[0].Text, "module not found") {
		t.Fatalf("expected module not found error, got: %s", result.Content[0].Text)
	}
}

// TestAddServiceCreatesViaAgentCreateFlow downloads a real agent from GitHub
// and runs its Create flow end to end. Opt-in like CODEFLY_GITOPS_K3D_QUALIFY
// (docs/commands.md) since it requires network access.
func TestAddServiceCreatesViaAgentCreateFlow(t *testing.T) {
	if os.Getenv("CODEFLY_MCP_AGENT_QUALIFY") != "1" {
		t.Skip("set CODEFLY_MCP_AGENT_QUALIFY=1 to run (downloads an agent from GitHub)")
	}

	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	text := callTool(t, server, ctx, "add_service", `{"module":"backend","name":"store","agent":"postgres"}`).Content[0].Text
	if !strings.Contains(text, `"status": "created"`) {
		t.Fatalf("expected status created, got: %s", text)
	}

	svcDir := filepath.Join("modules", "backend", "services", "store")
	if _, err := os.Stat(filepath.Join(svcDir, "service.codefly.yaml")); err != nil {
		t.Fatalf("expected service.codefly.yaml to exist: %v", err)
	}

	svc, err := resources.LoadServiceFromDir(ctx, svcDir)
	if err != nil {
		t.Fatalf("cannot load created service: %v", err)
	}
	if svc.Agent.Name != "postgres" {
		t.Errorf("expected agent name postgres, got %q", svc.Agent.Name)
	}
	if svc.Agent.Publisher == "" {
		t.Error("expected agent publisher to be set")
	}
	if _, err := semver.Parse(svc.Agent.Version); err != nil {
		t.Errorf("expected agent version to be valid semver, got %q: %v", svc.Agent.Version, err)
	}
}
