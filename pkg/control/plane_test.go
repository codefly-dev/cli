package control

import (
	"context"
	"os"
	"path/filepath"
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

	inv, err := New().Inventory(context.Background())
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

func TestFlowStatusIdleWhenNothingRunning(t *testing.T) {
	status, err := New().FlowStatus(context.Background())
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
