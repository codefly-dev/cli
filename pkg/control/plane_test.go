package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal on-disk workspace (modules layout) — one module, one service backed
// by the go-grpc agent. Enough to exercise name enumeration without installing
// any plugin (Inventory only parses YAML).
const (
	// A valid workspace: the only declared profile (saas) is empty, so it needs
	// no service inventory to validate and the workspace loads cleanly. Core
	// validates every declared profile's exclusions at load time, so a fixture
	// meant to exercise Inventory or a good run must not declare a broken
	// profile — that path is covered separately below.
	fixtureWorkspaceYAML = `name: demo
layout: modules
modules:
    - name: backend
run-profiles:
    saas: {}
`
	fixtureModuleYAML = `kind: module
name: backend
services:
    - name: api
`
	fixtureServiceYAML = `kind: service
name: api
version: 0.0.0
module: backend
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
`
)

// writeWorkspace lays the default (valid) fixture on disk and returns its root.
func writeWorkspace(t *testing.T) string {
	t.Helper()
	return writeWorkspaceWith(t, fixtureWorkspaceYAML)
}

// writeWorkspaceWith lays the fixture on disk using the supplied
// workspace.codefly.yaml, so a test can inject its own run-profiles block.
func writeWorkspaceWith(t *testing.T, workspaceYAML string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                            workspaceYAML,
		"modules/backend/module.codefly.yaml":               fixtureModuleYAML,
		"modules/backend/services/api/service.codefly.yaml": fixtureServiceYAML,
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

func TestInventoryListsWorkspaceResources(t *testing.T) {
	t.Chdir(writeWorkspace(t))

	plane := New()
	t.Cleanup(func() { _ = plane.Close() })
	inv, err := plane.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Workspace != "demo" {
		t.Errorf("workspace = %q, want demo", inv.Workspace)
	}
	if len(inv.Modules) != 1 || inv.Modules[0] != "backend" {
		t.Errorf("modules = %v, want [backend]", inv.Modules)
	}
	if len(inv.Services) != 1 || inv.Services[0] != "backend/api" {
		t.Errorf("services = %v, want [backend/api]", inv.Services)
	}
	if len(inv.Agents) != 1 || inv.Agents[0] != "codefly.ai/go-grpc" {
		t.Errorf("agents = %v, want [codefly.ai/go-grpc]", inv.Agents)
	}
}

func TestNewAtKeepsExplicitRootAfterWorkingDirectoryChanges(t *testing.T) {
	root := writeWorkspace(t)
	serviceDir := filepath.Join(root, "modules", "backend", "services", "api")
	plane, err := NewAt(serviceDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	t.Chdir(t.TempDir())
	inv, err := plane.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Workspace != "demo" {
		t.Fatalf("workspace = %q, want demo", inv.Workspace)
	}
}

func TestFlowStatusIdleWhenNothingRunning(t *testing.T) {
	plane := New()
	t.Cleanup(func() { _ = plane.Close() })
	status, err := plane.FlowStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FlowIdle {
		t.Errorf("state = %q, want idle", status.State)
	}
	if len(status.Services) != 0 {
		t.Errorf("services = %v, want none running", status.Services)
	}
}

// The run request itself carries the invalid selection: an undeclared profile
// name or an explicit exclusion that names a service the workspace does not
// have. Core's resolver rejects both, and Run must surface that before any
// agent starts.
func TestRunRejectsInvalidExclusionsBeforeStartingFlow(t *testing.T) {
	plane, err := NewAt(writeWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	tests := []struct {
		name    string
		request RunRequest
		want    string
	}{
		{
			name:    "undeclared profile",
			request: RunRequest{Service: "backend/api", Profile: "missing"},
			want:    `run profile "missing" is not declared`,
		},
		{
			name:    "explicit service",
			request: RunRequest{Service: "backend/api", Exclude: []string{"backend/missing"}},
			want:    `unknown service "backend/missing"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertRunRejected(t, plane, tt.request, tt.want)
		})
	}
}

// A declared profile that names a service or workspace configuration the
// workspace does not have is rejected when the workspace loads (core validates
// every declared profile up front), so Run fails before starting a flow.
func TestRunRejectsWorkspaceWithInvalidRunProfile(t *testing.T) {
	tests := []struct {
		name          string
		workspaceYAML string
		want          string
	}{
		{
			name: "unknown service",
			workspaceYAML: `name: demo
layout: modules
modules:
    - name: backend
run-profiles:
    local:
        exclude-dependencies:
            - backend/missing
`,
			want: `unknown service "backend/missing"`,
		},
		{
			name: "unknown workspace configuration",
			workspaceYAML: `name: demo
layout: modules
modules:
    - name: backend
run-profiles:
    local:
        exclude-workspace-configurations:
            - missing-auth
`,
			want: `unknown workspace configuration "missing-auth"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plane, err := NewAt(writeWorkspaceWith(t, tt.workspaceYAML))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = plane.Close() })
			assertRunRejected(t, plane, RunRequest{Service: "backend/api"}, tt.want)
		})
	}
}

// assertRunRejected runs the request, requires it to fail with want in the
// message, and confirms the plane is left idle (no flow was started).
func assertRunRejected(t *testing.T, plane Plane, request RunRequest, want string) {
	t.Helper()
	_, err := plane.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Run error = %v, want %q", err, want)
	}
	status, statusErr := plane.FlowStatus(context.Background())
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if status.State != FlowIdle {
		t.Fatalf("flow state = %q, want idle", status.State)
	}
}
