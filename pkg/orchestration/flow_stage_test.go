package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
)

func TestTestFlowUsesRuntimeStageAcrossMixedCycle(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/module-layout")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "modules/management/services/organization/service.codefly.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte("\nservice-dependencies:\n - name: frontend\n   module: web\n   kind: build\n")...)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(root, "modules/web/services/frontend/service.codefly.yaml")
	payload, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(strings.Replace(string(payload), "- name: gateway", "- kind: runtime\n      name: gateway", 1))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dependencies.OrderTo(ctx, "web/frontend"); err == nil {
		t.Fatal("fixture must have a union cycle")
	}
	for _, mode := range []Mode{RunMode, TestMode, SyncMode, DeployMode, SnapshotMode} {
		t.Run(string(mode), func(t *testing.T) {
			flow := &Flow{world: &World{Mode: mode, Dependencies: dependencies}}
			if err := flow.selectDependencyStage(); err != nil {
				t.Fatal(err)
			}
			order, err := flow.world.Dependencies.OrderTo(ctx, "web/frontend")
			if err != nil {
				t.Fatal(err)
			}
			if len(order) != 3 {
				t.Fatalf("lost runtime dependencies: %v", order)
			}
			order, err = flow.world.Dependencies.OrderTo(ctx, "management/organization")
			if err != nil {
				t.Fatal(err)
			}
			if len(order) != 0 {
				t.Fatalf("build-only dependencies started at runtime: %v", order)
			}
		})
	}
	build := &Flow{world: &World{Mode: BuildMode, Dependencies: dependencies}}
	if err := build.selectDependencyStage(); err != nil {
		t.Fatal(err)
	}
	order, err := build.world.Dependencies.OrderTo(ctx, "web/frontend")
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 0 {
		t.Fatalf("runtime dependency entered build stage: %v", order)
	}

}
