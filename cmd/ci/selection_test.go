package ci

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestBuildPlanExpandsTransitiveDependentsInDependencyOrder(t *testing.T) {
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{
		RepoRoot: root,
		ChangedFiles: []string{
			"modules/management/services/organization/code/organization.go",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"management/organization:direct",
		"billing/accounts:dependent",
		"web/gateway:dependent",
		"web/frontend:dependent",
	}
	if got := servicePlanSummary(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("service plan = %v, want %v", got, want)
	}
}

func TestBuildPlanLimitsModuleChangeToModuleServices(t *testing.T) {
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{
		RepoRoot:     root,
		ChangedFiles: []string{"modules/web/module.codefly.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"web/gateway:direct", "web/frontend:direct"}
	if got := servicePlanSummary(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("service plan = %v, want %v", got, want)
	}
}

func TestBuildPlanResolvesRepositoryPathThroughWorkspaceSymlink(t *testing.T) {
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	if err := os.Symlink(filepath.Join("modules", "management"), filepath.Join(root, "module")); err != nil {
		t.Fatalf("create module alias: %v", err)
	}
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{
		RepoRoot:     root,
		ChangedFiles: []string{"module/services/organization/code/deleted.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"management/organization:direct",
		"billing/accounts:dependent",
		"web/gateway:dependent",
		"web/frontend:dependent",
	}
	if got := servicePlanSummary(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("service plan = %v, want %v", got, want)
	}
}

func TestBuildPlanWorkspaceConfigurationSelectsAll(t *testing.T) {
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{
		RepoRoot:     root,
		ChangedFiles: []string{"workspace.codefly.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"management/organization:global",
		"billing/accounts:global",
		"web/gateway:global",
		"web/frontend:global",
	}
	if got := servicePlanSummary(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("service plan = %v, want %v", got, want)
	}
	for _, service := range plan.Services {
		if len(service.Paths) != 0 {
			t.Fatalf("global service %s repeats changed paths: %v", service.Service, service.Paths)
		}
		if !reflect.DeepEqual(service.Reasons, []string{"workspace-level input changed"}) {
			t.Fatalf("global service %s has noisy reasons: %v", service.Service, service.Reasons)
		}
	}
}

func TestBuildPlanGlobalSelectionSupersedesDirectPathDetails(t *testing.T) {
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{
		RepoRoot: root,
		ChangedFiles: []string{
			"modules/management/services/organization/code/organization.go",
			"workspace.codefly.yaml",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range plan.Services {
		if service.Classification != "global" || len(service.Paths) != 0 {
			t.Fatalf("service %s retained subordinate selection: %+v", service.Service, service)
		}
		if !reflect.DeepEqual(service.Reasons, []string{"workspace-level input changed"}) {
			t.Fatalf("global service %s has noisy reasons: %v", service.Service, service.Reasons)
		}
	}
}

func TestBuildPlanLibraryChangeSelectsConsumers(t *testing.T) {
	root, workspace := loadPlanFixture(t, "../../../core/resources/testdata/workspaces/with-library")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{
		RepoRoot:     root,
		ChangedFiles: []string{"libraries/shared-models/go/model.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"with-library/api:direct"}
	if got := servicePlanSummary(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("service plan = %v, want %v", got, want)
	}
}

func TestBuildPlanIgnoresDocumentationAndProviderMetadata(t *testing.T) {
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{
		RepoRoot:     root,
		ChangedFiles: []string{"docs/ci.md", ".github/CODEOWNERS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Services) != 0 {
		t.Fatalf("documentation-only plan selected services: %v", servicePlanSummary(plan))
	}
}

func TestBuildPlanInCIWithoutBoundsFailsClosedToAll(t *testing.T) {
	t.Setenv("CI", "true")
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Services) != 4 {
		t.Fatalf("fallback selected %d services, want 4", len(plan.Services))
	}
	for _, service := range plan.Services {
		if service.Classification != "global" {
			t.Fatalf("fallback classification for %s = %s", service.Service, service.Classification)
		}
	}
	if plan.SelectionReason == "" {
		t.Fatal("fallback plan omitted its selection reason")
	}
}

func TestParseNameStatusZIncludesBothRenamePaths(t *testing.T) {
	raw := []byte("M\x00modules/api/code/a.go\x00R100\x00old/path.go\x00new/path.go\x00D\x00gone.go\x00")
	want := []string{"gone.go", "modules/api/code/a.go", "new/path.go", "old/path.go"}
	if got := parseNameStatusZ(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseNameStatusZ = %v, want %v", got, want)
	}
}

func loadPlanFixture(t *testing.T, relative string) (string, *resources.Workspace) {
	t.Helper()
	t.Setenv("CI", "")
	source, err := filepath.Abs(relative)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(source)); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	return root, workspace
}

func servicePlanSummary(plan *Plan) []string {
	result := make([]string, 0, len(plan.Services))
	for _, service := range plan.Services {
		result = append(result, service.Service+":"+service.Classification)
	}
	return result
}
