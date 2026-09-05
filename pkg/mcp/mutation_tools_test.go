package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/codefly-dev/core/resources"
)

func TestSynchronizedBufferConcurrentSnapshot(t *testing.T) {
	var buffer synchronizedBuffer
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 1_000; i++ {
				_, _ = fmt.Fprintf(&buffer, "%d:%d\n", worker, i)
				_ = buffer.String()
			}
		}(worker)
	}
	wg.Wait()
	if got := buffer.String(); !strings.Contains(got, "0:0\n") {
		t.Fatalf("buffer lost written output")
	}
}

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
