package ci

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestReferenceSelectionUsesCoreServicePaths(t *testing.T) {
	root, _ := loadSchedulerFixture(t)
	path := filepath.Join(root, "modules/management/module.codefly.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(payload), "- name: organization", "- name: organization\n      path: organization", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	plan, err := BuildPlan(ctx, workspace, PlanOptions{Base: "HEAD", ChangedFiles: []string{"modules/management/services/organization/service.codefly.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Services) != 5 {
		t.Fatalf("lost reference consumers: %v", servicePlanSummary(plan))
	}
}

func TestReferenceSelectionKeepsExternalInputsInTheirRepository(t *testing.T) {
	root, _ := loadSchedulerFixture(t)
	external := t.TempDir()
	if err := os.CopyFS(external, os.DirFS(filepath.Join(root, "modules/management"))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workspace.codefly.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(payload), "- name: management", "- name: management\n      path: "+external, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	plan, err := BuildPlan(ctx, workspace, PlanOptions{Base: "HEAD", ChangedFiles: []string{"modules/web/services/frontend/service.codefly.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := servicePlanSummary(plan); !reflect.DeepEqual(got, []string{"web/frontend:direct"}) {
		t.Fatalf("unrelated external agents selected: %v", got)
	}
	_, err = BuildPlan(ctx, workspace, PlanOptions{Base: "HEAD", ChangedFiles: []string{filepath.Join(external, "services/organization/service.codefly.yaml")}})
	if err == nil || !strings.Contains(err.Error(), "owning repository") {
		t.Fatalf("external change used wrong revision: %v", err)
	}
}
