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
	root, workspace := loadPlanFixture(t, "testdata/with-library")
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

func loadPlanFixture(t testing.TB, relative string) (string, *resources.Workspace) {
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

func loadComposedPlanFixture(t *testing.T) (string, *resources.Workspace) {
	t.Helper()
	t.Setenv("CI", "")
	root := t.TempDir()
	writeCacheTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), "name: composed\nlayout: modules\nmodules:\n  - name: app\n")
	names := []string{"accounts", "gateway", "frontend", "billing", "jobs", "mail", "search", "storage"}
	module := "kind: module\nname: app\nservices:\n"
	for _, name := range names {
		module += "  - name: " + name + "\n"
		service := "kind: service\nname: " + name + "\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: golang\n  version: 0.0.1\n  publisher: codefly.dev\n"
		if name == "frontend" {
			service += "service-dependencies:\n  - name: gateway\n"
		}
		if name == "gateway" {
			service += "service-dependencies:\n  - name: accounts\n"
		}
		writeCacheTestFile(t, filepath.Join(root, "module", "services", name, "service.codefly.yaml"), service)
	}
	writeCacheTestFile(t, filepath.Join(root, "module", "module.codefly.yaml"), module)
	writeCacheTestFile(t, filepath.Join(root, "module", "services", "frontend", "code", "src", "example.ts"), "export const example = 1;\n")
	if err := os.Mkdir(filepath.Join(root, "modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../module", filepath.Join(root, "modules", "app")); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return root, workspace
}

func TestDeclarationEditDoesNotSelectUnrelatedSuites(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{RepoRoot: root, ChangedFiles: []string{"modules/web/services/frontend/service.codefly.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := servicePlanSummary(plan); !reflect.DeepEqual(got, []string{"web/frontend:direct"}) {
		t.Fatalf("unrelated suites selected: %v", got)
	}
	tasks, err := buildScheduledTasks(context.Background(), workspace, plan, ScheduleOptions{Phase: "test", Suite: "integration", LockDependencyClosure: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || len(tasks[0].resources) != 4 {
		t.Fatalf("test targets/runtime resources: %+v", tasks)
	}
}

func TestRemovedDeclarationUsesReferenceDependents(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	// Remove the organization dependency from both consumers before deleting it.
	for _, relative := range []string{"modules/billing/services/accounts/service.codefly.yaml", "modules/management/services/consumer/service.codefly.yaml"} {
		path := filepath.Join(root, relative)
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(string(payload), "service-dependencies:")
		end := strings.Index(string(payload)[start:], "endpoints:")
		replacement := string(payload)[:start]
		if end >= 0 {
			replacement += string(payload)[start+end:]
		}
		if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "modules/management/module.codefly.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(strings.Replace(string(payload), "    - name: organization\n", "", 1))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "modules/management/services/organization")); err != nil {
		t.Fatal(err)
	}
	workspace, err = resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{Base: "HEAD", ChangedFiles: []string{"modules/management/services/organization/service.codefly.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	got := servicePlanSummary(plan)
	want := []string{"billing/accounts:dependent", "management/consumer:dependent", "web/gateway:dependent", "web/frontend:dependent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed service coverage = %v, want %v", got, want)
	}
}

func TestCommittedRemovalRequiresReferenceContainingDeclaration(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	// Remove the organization dependency from both consumers before deleting it.
	for _, relative := range []string{"modules/billing/services/accounts/service.codefly.yaml", "modules/management/services/consumer/service.codefly.yaml"} {
		path := filepath.Join(root, relative)
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(string(payload), "service-dependencies:")
		end := strings.Index(string(payload)[start:], "endpoints:")
		replacement := string(payload)[:start]
		if end >= 0 {
			replacement += string(payload)[start+end:]
		}
		if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "modules/management/module.codefly.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(strings.Replace(string(payload), "    - name: organization\n", "", 1))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "modules/management/services/organization")); err != nil {
		t.Fatal(err)
	}
	workspace, err = resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "remove producer")
	for _, base := range []string{"", "HEAD"} {
		_, err := BuildPlan(context.Background(), workspace, PlanOptions{Base: base, ChangedFiles: []string{"modules/management/services/organization/service.codefly.yaml"}})
		if err == nil || !strings.Contains(err.Error(), "supply --base with a revision containing the declaration") {
			t.Fatalf("base %q accepted unresolvable deletion: %v", base, err)
		}
	}
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{Base: "HEAD~1", ChangedFiles: []string{"modules/management/services/organization/service.codefly.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	got := servicePlanSummary(plan)
	want := []string{"billing/accounts:dependent", "management/consumer:dependent", "web/gateway:dependent", "web/frontend:dependent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed service coverage = %v, want %v", got, want)
	}
}
