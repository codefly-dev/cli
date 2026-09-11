package ci

import (
	"context"
	"fmt"
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

func TestBuildPlanDerivedIntegrityInput(t *testing.T) {
	root, workspace := loadComposedPlanFixture(t)
	ctx := context.Background()
	source := "module/services/frontend/code/src/example.ts"
	manifest := "module/tools/base-manifest.json"
	baseline, err := BuildPlan(ctx, workspace, PlanOptions{RepoRoot: root, ChangedFiles: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{manifest, "modules/app/tools/base-manifest.json"} {
		plan, err := BuildPlan(ctx, workspace, PlanOptions{RepoRoot: root, ChangedFiles: []string{source, path}})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(plan.Services, baseline.Services) || len(plan.Services) != 1 {
			t.Fatalf("source plus manifest changed service coverage: %+v", plan.Services)
		}
		want := []IntegrityInput{{Module: "app", Path: path, Phase: "verify", Reason: "base hash and ownership index is integrity-owned; source paths determine affected services"}}
		if !reflect.DeepEqual(plan.IntegrityInputs, want) {
			t.Fatalf("integrity inputs = %+v, want %+v", plan.IntegrityInputs, want)
		}
		for _, phase := range []string{"test", "build"} {
			reporter := fixedCIReporter(t, plan)
			if err := prepareCIReportTasks(ctx, workspace, plan, ScheduleOptions{Jobs: 1, Phase: phase, Reporter: reporter}); err != nil {
				t.Fatal(err)
			}
			report := reporter.Finalize(nil)
			if len(report.Tasks) != 1 || report.Tasks[0].Service != "app/frontend" {
				t.Fatalf("%s tasks = %+v, want only frontend", phase, report.Tasks)
			}
		}
	}
	for _, path := range []string{
		"module/tools/build.sh", "module/tools/settings.json", "module/tools/base-integrity-allow.json",
		"module/contracts/api.json", "module/module.codefly.yaml", "module/configurations/runtime.yaml",
		"module/deployment/topology.json", "module/tools/base-manifest.json.backup", "tools/base-manifest.json",
	} {
		t.Run(path, func(t *testing.T) {
			plan, err := BuildPlan(ctx, workspace, PlanOptions{RepoRoot: root, ChangedFiles: []string{path, manifest}})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Services) != 8 {
				t.Fatalf("shared input selected %d services, want 8", len(plan.Services))
			}
		})
	}
}

func TestBuildPlanGitAndProviderIntegrityChangesAgree(t *testing.T) {
	for _, operation := range []string{"modify", "delete", "rename", "move"} {
		t.Run(operation, func(t *testing.T) {
			root, workspace := loadComposedPlanFixture(t)
			manifest := "module/tools/base-manifest.json"
			source := "module/services/frontend/code/src/example.ts"
			writeCacheTestFile(t, filepath.Join(root, manifest), `{"files":{}}`)
			runCacheTestGit(t, root, "init")
			runCacheTestGit(t, root, "add", ".")
			runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
			changed := []string{source, manifest}
			wantServices := 1
			switch operation {
			case "modify":
				writeCacheTestFile(t, filepath.Join(root, source), "export const example = 2;\n")
			case "delete":
				if err := os.Remove(filepath.Join(root, source)); err != nil {
					t.Fatal(err)
				}
			case "rename", "move":
				destination := "module/services/frontend/code/src/renamed.ts"
				if operation == "move" {
					destination = "module/services/accounts/code/example.ts"
					wantServices = 3
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, destination)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(root, source), filepath.Join(root, destination)); err != nil {
					t.Fatal(err)
				}
				changed = append(changed, destination)
			}
			writeCacheTestFile(t, filepath.Join(root, manifest), "{\"files\":{}}\n")
			runCacheTestGit(t, root, "add", ".")
			local, err := BuildPlan(context.Background(), workspace, PlanOptions{RepoRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", operation)
			t.Setenv("CI", "true")
			hosted, err := BuildPlan(context.Background(), workspace, PlanOptions{RepoRoot: root, Base: "HEAD~1"})
			if err != nil {
				t.Fatal(err)
			}
			provider, err := BuildPlan(context.Background(), workspace, PlanOptions{RepoRoot: root, ChangedFiles: changed})
			if err != nil {
				t.Fatal(err)
			}
			for _, plan := range []*Plan{local, hosted} {
				if !reflect.DeepEqual(plan.Services, provider.Services) || !reflect.DeepEqual(plan.IntegrityInputs, provider.IntegrityInputs) || len(plan.Services) != wantServices {
					t.Fatalf("%s: discovered %+v, provider %+v", operation, plan, provider)
				}
			}
		})
	}
}

func TestBuildPlanAllRetainsIntegrityInputs(t *testing.T) {
	for _, provider := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider=%t", provider), func(t *testing.T) {
			root, workspace := loadComposedPlanFixture(t)
			path := "module/tools/base-manifest.json"
			writeCacheTestFile(t, filepath.Join(root, path), `{"files":{}}`)
			runCacheTestGit(t, root, "init")
			runCacheTestGit(t, root, "add", ".")
			runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "baseline")
			if err := os.Remove(filepath.Join(root, path)); err != nil {
				t.Fatal(err)
			}
			options := PlanOptions{RepoRoot: root, All: true}
			if provider {
				options.ChangedFiles = []string{path}
			}
			plan, err := BuildPlan(context.Background(), workspace, options)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Services) != 8 || len(plan.IntegrityInputs) != 1 {
				t.Fatalf("plan = %+v", plan)
			}
			phases, err := plan.runPhases([]string{"test"})
			if err != nil || !reflect.DeepEqual(phases, []string{"verify", "test"}) {
				t.Fatalf("phases=%v err=%v", phases, err)
			}
			if err := executeCIPhase(context.Background(), fixedCIReporter(t, plan), workspace, plan, "verify", nil, true); err == nil {
				t.Fatal("--all bypassed deleted manifest verification")
			}
		})
	}
}

func TestBuildPlanSymlinkedToolsRetainsIntegrityOwner(t *testing.T) {
	for _, exists := range []bool{false, true} {
		for _, path := range []string{"module/metadata/base-manifest.json", "module/tools/base-manifest.json", "modules/app/tools/base-manifest.json"} {
			t.Run(fmt.Sprintf("exists=%t/%s", exists, path), func(t *testing.T) {
				root, workspace := loadComposedPlanFixture(t)
				if err := os.Mkdir(filepath.Join(root, "module", "metadata"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("metadata", filepath.Join(root, "module", "tools")); err != nil {
					t.Fatal(err)
				}
				if exists {
					writeCacheTestFile(t, filepath.Join(root, "module", "metadata", "base-manifest.json"), `{"files":{}}`)
				}
				plan, err := BuildPlan(context.Background(), workspace, PlanOptions{RepoRoot: root, ChangedFiles: []string{path}})
				if err != nil {
					t.Fatal(err)
				}
				if len(plan.Services) != 0 || len(plan.IntegrityInputs) != 1 || plan.IntegrityInputs[0].Module != "app" {
					t.Fatalf("plan = %+v", plan)
				}
				if !exists {
					if err := runVerifyWorkspace(context.Background(), workspace, plan); err == nil {
						t.Fatal("deleted linked manifest passed verification")
					}
				}
			})
		}
	}
}

func TestUnknownChangesCannotPassIntegrityGate(t *testing.T) {
	for _, scenario := range []string{"discovery failure", "CI without bounds"} {
		t.Run(scenario, func(t *testing.T) {
			root, workspace := loadComposedPlanFixture(t)
			if scenario == "CI without bounds" {
				t.Setenv("CI", "true")
			}
			plan, err := BuildPlan(context.Background(), workspace, PlanOptions{RepoRoot: root, All: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Services) != 8 || plan.IntegrityError == "" {
				t.Fatalf("plan = %+v", plan)
			}
			phases, err := plan.runPhases([]string{"test"})
			if err != nil || !reflect.DeepEqual(phases, []string{"verify", "test"}) {
				t.Fatalf("phases=%v err=%v", phases, err)
			}
			reporter := fixedCIReporter(t, plan)
			if err := executeCIPhase(context.Background(), reporter, workspace, plan, "verify", nil, true); err == nil {
				t.Fatal("unknown integrity inputs passed verification")
			}
			report := reporter.Finalize(nil)
			if len(report.Tasks) != 1 || report.Tasks[0].Status != reportStatusFailed {
				t.Fatalf("tasks = %+v", report.Tasks)
			}
		})
	}
}

func TestDeclarationChangesRetainRemovedEdgeCoverage(t *testing.T) {
	for _, path := range []string{
		"modules/web/services/frontend/service.codefly.yaml",
		"modules/removed/services/deleted/service.codefly.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
			plan, err := BuildPlan(context.Background(), workspace, PlanOptions{RepoRoot: root, ChangedFiles: []string{path}})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Services) != 4 {
				t.Fatalf("declaration change lost coverage: %v", servicePlanSummary(plan))
			}
			for _, service := range plan.Services {
				if service.Classification != "global" {
					t.Fatalf("classification: %v", service)
				}
			}
		})
	}
}
