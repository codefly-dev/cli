package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/conformance"
)

// These lock the behavior of run_service/test_service/flow_status/stop_flow
// now that they go through the control plane instead of shelling out to a
// `codefly` binary on PATH.

func TestRunServiceRequiresModuleAndService(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	text := callTool(t, server, ctx, "run_service", `{}`).Content[0].Text
	if !strings.Contains(text, "required") {
		t.Errorf("run_service with no args = %q, want it to mention required args", text)
	}
}

func TestRunServiceUnknownServiceReturnsPlaneError(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	text := callTool(t, server, ctx, "run_service", `{"module":"backend","service":"nope","wait":"false"}`).Content[0].Text
	if !strings.HasPrefix(text, "run backend/nope failed:") {
		t.Errorf("run_service for unknown service = %q, want prefix %q", text, "run backend/nope failed:")
	}
}

func TestFlowStatusIdleWithoutRun(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	text := callTool(t, server, ctx, "flow_status", `{}`).Content[0].Text
	if !strings.Contains(text, `"state": "idle"`) {
		t.Errorf("flow_status without a run = %q, want it to report idle", text)
	}
}

func TestStopFlowWhenNothingRunning(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	text := callTool(t, server, ctx, "stop_flow", `{}`).Content[0].Text
	if text != "nothing running" {
		t.Errorf("stop_flow with nothing running = %q, want %q", text, "nothing running")
	}
}

// flow_status/stop_flow must accept an explicit flow_id (the flow_id a
// run_service response returns) rather than only ever inferring "whichever
// flow is currently active" — the latter silently targets the wrong flow the
// moment more than one run_service call is in flight (run_service's
// wait="false" option makes that reachable). These are hermetic plumbing
// checks; pkg/control's TestFlowStatusWithExplicitFlowIDDoesNotLeakADifferentFlowsOutcome
// covers the actual cross-flow isolation, since exercising two real
// concurrently-running flows here would require Docker/installed agents.
func TestFlowStatusAcceptsExplicitFlowID(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	text := callTool(t, server, ctx, "flow_status", `{"flow_id":"backend/api"}`).Content[0].Text
	if !strings.Contains(text, `"state": "idle"`) {
		t.Errorf("flow_status with an explicit flow_id and nothing running = %q, want idle", text)
	}
}

func TestStopFlowAcceptsExplicitFlowID(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	text := callTool(t, server, ctx, "stop_flow", `{"flow_id":"backend/api"}`).Content[0].Text
	if text != "nothing running" {
		t.Errorf("stop_flow with an explicit flow_id and nothing running = %q, want %q", text, "nothing running")
	}
}

func TestCloseCancelsRunContext(t *testing.T) {
	t.Chdir(writeMCPWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if server.runCtx.Err() == nil {
		t.Error("runCtx.Err() == nil after Close, want it cancelled")
	}
}

func TestSubprocessToolsUseCurrentBinary(t *testing.T) {
	path, err := currentCLI()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("currentCLI() = %q, which does not exist: %v", path, err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("currentCLI() = %q, which is not executable", path)
	}
}

const (
	postgresQualifyWorkspaceYAML = `name: rundep
layout: modules
modules:
    - name: infra
`
	postgresQualifyModuleYAML = `kind: module
name: infra
services:
    - name: db
`
	postgresQualifyServiceYAML = `kind: service
name: db
version: 0.0.0
module: infra
agent:
    kind: codefly:service
    name: postgres
    version: 0.0.130
    publisher: codefly.dev
endpoints:
    - name: tcp
`
)

func writePostgresQualifyWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                         postgresQualifyWorkspaceYAML,
		"modules/infra/module.codefly.yaml":              postgresQualifyModuleYAML,
		"modules/infra/services/db/service.codefly.yaml": postgresQualifyServiceYAML,
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

// TestRunServiceLifecycleThroughPlane proves run_service/flow_status/stop_flow
// drive a real flow end to end. It spins up an actual postgres container, so
// it is opt-in and requires Docker.
func TestRunServiceLifecycleThroughPlane(t *testing.T) {
	conformance.Gate(t, "linux-amd64-docker-mcp-run")
	t.Chdir(writePostgresQualifyWorkspace(t))
	ctx := context.Background()
	server, err := NewServer(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	runResult := callTool(t, server, ctx, "run_service", `{"module":"infra","service":"db","wait":"true","timeout_seconds":"180"}`)
	var run struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(runResult.Content[0].Text), &run); err != nil {
		t.Fatalf("parse run_service result %q: %v", runResult.Content[0].Text, err)
	}
	if run.State != "running" {
		t.Fatalf("run_service state = %q, want %q", run.State, "running")
	}

	statusResult := callTool(t, server, ctx, "flow_status", `{}`)
	if !strings.Contains(statusResult.Content[0].Text, `"state": "running"`) {
		t.Fatalf("flow_status after run = %q, want running", statusResult.Content[0].Text)
	}

	stopResult := callTool(t, server, ctx, "stop_flow", `{"destroy":"true"}`)
	if stopResult.Content[0].Text != "stopped" {
		t.Fatalf("stop_flow = %q, want %q", stopResult.Content[0].Text, "stopped")
	}

	idleResult := callTool(t, server, ctx, "flow_status", `{}`)
	if !strings.Contains(idleResult.Content[0].Text, `"state": "idle"`) {
		t.Fatalf("flow_status after stop = %q, want idle", idleResult.Content[0].Text)
	}
}
