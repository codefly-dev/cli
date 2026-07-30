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
	fixtureWorkspaceYAML = `name: demo
layout: modules
modules:
    - name: backend
run-profiles:
    unknown-service:
        exclude-dependencies:
            - backend/missing
    unknown-configuration:
        exclude-workspace-configurations:
            - missing-auth
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

// writeWorkspace lays the fixture on disk and returns its root.
func writeWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                            fixtureWorkspaceYAML,
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
			name:    "profile",
			request: RunRequest{Service: "backend/api", Profile: "missing"},
			want:    `unknown run profile "missing"`,
		},
		{
			name:    "profile service",
			request: RunRequest{Service: "backend/api", Profile: "unknown-service"},
			want:    `resolve excluded dependency "backend/missing"`,
		},
		{
			name:    "explicit service",
			request: RunRequest{Service: "backend/api", Exclude: []string{"backend/missing"}},
			want:    `resolve excluded dependency "backend/missing"`,
		},
		{
			name:    "workspace configuration",
			request: RunRequest{Service: "backend/api", Profile: "unknown-configuration"},
			want:    `excludes unknown workspace configuration "missing-auth"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := plane.Run(context.Background(), tt.request)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run error = %v, want %q", err, tt.want)
			}
			status, statusErr := plane.FlowStatus(context.Background())
			if statusErr != nil {
				t.Fatal(statusErr)
			}
			if status.State != FlowIdle {
				t.Fatalf("flow state = %q, want idle", status.State)
			}
		})
	}
}
