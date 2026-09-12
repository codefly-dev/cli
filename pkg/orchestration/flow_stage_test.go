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
	for _, mode := range []Mode{RunMode, TestMode, SyncMode, DeployMode} {
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
	snapshot := &Flow{world: &World{Mode: SnapshotMode, Dependencies: dependencies}}
	snapshot.originService, err = dependencies.ServiceFromUnique("web/frontend")
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.selectDependencyStage(); err != nil {
		t.Fatal(err)
	}
	required, err := snapshot.managerDependencies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(required) != 3 {
		t.Fatalf("snapshot lost managers: %v", required)
	}
	snapshot.originService, err = dependencies.ServiceFromUnique("management/organization")
	if err != nil {
		t.Fatal(err)
	}
	required, err = snapshot.managerDependencies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(required) != 3 {
		t.Fatalf("snapshot lost build-only managers: %v", required)
	}
	for _, alone := range []bool{false, true} {
		policy, err := NewSnapshotPolicy(ctx, snapshot.world.Dependencies, nil)
		if err != nil {
			t.Fatal(err)
		}
		policy.standAlone = alone
		if err := policy.Restrict(ctx, "web/frontend"); err != nil {
			t.Fatal(err)
		}
		positions := map[ActionType]map[string]int{}
		for index, action := range policy.actions {
			if positions[action.Type] == nil {
				positions[action.Type] = map[string]int{}
			}
			positions[action.Type][action.Service] = index
		}
		if alone {
			if len(policy.actions) != 4 {
				t.Fatalf("standalone snapshot actions: %v", policy.actions)
			}
			continue
		}
		if len(policy.actions) != 16 {
			t.Fatalf("snapshot omitted actions: %v", policy.actions)
		}
		if positions[BuilderBuild]["web/frontend"] >= positions[BuilderBuild]["management/organization"] {
			t.Fatal("snapshot built organization before its build prerequisite frontend")
		}
		if positions[BuilderDeploy]["management/organization"] >= positions[BuilderDeploy]["web/frontend"] {
			t.Fatal("snapshot rendered frontend before its runtime prerequisite organization")
		}
		for _, built := range positions[BuilderBuild] {
			for _, rendered := range positions[BuilderDeploy] {
				if built >= rendered {
					t.Fatal("snapshot rendered before all artifacts were built")
				}
			}
		}
		if !policy.completed(policy.actions[len(policy.actions)-1]) {
			t.Fatal("snapshot does not stop after final render")
		}
	}
	policy, err := NewSnapshotPolicy(ctx, snapshot.world.Dependencies, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Restrict(ctx, "management/organization"); err != nil {
		t.Fatal(err)
	}
	if policy.completed(Action{Service: "management/organization", Type: BuilderDeploy}) {
		t.Fatal("snapshot stopped at its origin before rendering remaining consumers")
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

func TestSnapshotDoesNotAdvanceAfterIncompleteBuild(t *testing.T) {
	build := Action{Service: "app/producer", Type: BuilderBuild}
	render := Action{Service: "app/producer", Type: BuilderDeploy}
	policy := &SnapshotPolicy{actions: []Action{build, render}}
	for _, output := range []*OutputProperty{nil, {}, Pause()} {
		next, err := policy.next(build, output)
		if err == nil || len(next) != 0 {
			t.Fatalf("incomplete build advanced: %v, %v", next, err)
		}
	}
	next, err := policy.next(build, OnInit())
	if err != nil || len(next) != 1 || next[0] != render {
		t.Fatalf("completed build: %v, %v", next, err)
	}
	next, err = policy.next(render, OnInit())
	if err != nil || len(next) != 0 {
		t.Fatalf("completed render: %v, %v", next, err)
	}
}
