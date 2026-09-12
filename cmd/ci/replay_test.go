package ci

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/codefly-dev/core/resources"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReplayPreservesSelectionAndRejectsAlterations(t *testing.T) {
	for _, scenario := range []string{"roundtrip", "reasons", "paths", "tasks", "topology", "stage", "fingerprint", "revision", "content", "ignored", "all", "bounds", "schema"} {
		t.Run(scenario, func(t *testing.T) {
			root, workspace := loadSchedulerFixture(t)
			runCacheTestGit(t, root, "init")
			runCacheTestGit(t, root, "add", ".")
			runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
			options := PlanOptions{ChangedFiles: []string{"modules/management/services/organization/code/deleted.go"}}
			ctx := context.Background()
			plan, err := BuildPlan(ctx, workspace, options)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := buildReplayPlan(ctx, workspace, plan, ReplayInvocation{})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "reasons":
				saved.Selection.Services[0].Reasons = []string{"altered"}
			case "paths":
				saved.Selection.ChangedFiles = []string{"elsewhere"}
			case "tasks":
				saved.Selection.Services = saved.Selection.Services[:1]
			case "topology":
				saved.Tasks[len(saved.Tasks)-1].Prerequisites = []string{"altered/prerequisite"}
			case "stage":
				saved.Tasks[0].Stage = resources.StageBuild
			case "fingerprint":
				saved.Fingerprint = "altered"
			case "revision":
				saved.Candidate = "stale"
			case "content":
				writeCacheTestFile(t, filepath.Join(root, "untracked.go"), "package changed")
			case "ignored":
				writeCacheTestFile(t, filepath.Join(root, ".git", "info", "exclude"), "ignored.input\n")
				writeCacheTestFile(t, filepath.Join(root, "ignored.input"), "changed")
			case "all":
				options.All = true
			case "bounds":
				options = PlanOptions{}
			case "schema":
				saved.Schema = "future"
			}
			path := filepath.Join(t.TempDir(), "plan.json")
			payload, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			actual, err := readReplayPlan(ctx, workspace, path, &options, ReplayInvocation{})
			if scenario == "roundtrip" {
				if err != nil {
					t.Fatal(err)
				}
				actual.replay = nil
				if !reflect.DeepEqual(actual, plan) {
					t.Fatalf("selection changed: %#v", actual)
				}
			} else if err == nil {
				t.Fatal("accepted altered or incompatible plan")
			}
		})
	}
}

func TestReplayContentFollowsSymlinksAndHandlesBackLinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "source")
	writeCacheTestFile(t, target, "before")
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	before, err := replayContent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, target, "after")
	after, err := replayContent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("symlink target contents were not validated")
	}
	if err := os.Symlink(root, filepath.Join(root, "cycle")); err != nil {
		t.Fatal(err)
	}
	cyclic, err := replayContent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, target, "changed again")
	changed, err := replayContent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if cyclic == changed {
		t.Fatal("back-link hid changed source")
	}
}

func TestReplayContentRejectsCancellationAndSockets(t *testing.T) {
	root, err := os.MkdirTemp("", "ci-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := replayContent(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := replayContent(context.Background(), root); err == nil || !strings.Contains(err.Error(), "unsupported source file type") {
		t.Fatalf("socket: %v", err)
	}
}

func TestReplayRejectsChangedExternalModuleSource(t *testing.T) {
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
	payload = []byte(strings.Replace(string(payload), "- name: management", "- name: management\n      path: "+external, 1))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	options := PlanOptions{All: true, ChangedFiles: []string{"README.md"}}
	plan, err := BuildPlan(context.Background(), workspace, options)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := buildReplayPlan(context.Background(), workspace, plan, ReplayInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, filepath.Join(external, "services/organization/code/new.go"), "package changed")
	if _, err := readReplayPlan(context.Background(), workspace, file, &options, ReplayInvocation{}); err == nil || !strings.Contains(err.Error(), "contents changed") {
		t.Fatalf("external source change: %v", err)
	}
}

func TestReplayRejectsEndpointVisibilityViolation(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	path := filepath.Join(root, "modules/management/services/organization/service.codefly.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(strings.ReplaceAll(string(payload), "visibility: public", "visibility: internal\n      allow-modules: [management]"))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{All: true, ChangedFiles: []string{"README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildReplayPlan(context.Background(), workspace, plan, ReplayInvocation{}); err == nil || !strings.Contains(err.Error(), "visibility") {
		t.Fatalf("visibility validation: %v", err)
	}
}

func TestReplayBindsTasksAndDispatchesBuildPrerequisites(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	ctx := context.Background()
	options := PlanOptions{ChangedFiles: []string{"modules/web/services/frontend/code/index.js"}}
	invocation := ReplayInvocation{Phases: []string{"build"}, Suites: []string{"integration"}, RuntimeContext: "free"}
	plan, err := BuildPlan(ctx, workspace, options)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := buildReplayPlan(ctx, workspace, plan, invocation)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "plan.json")
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []ReplayInvocation{
		{Phases: []string{"lint"}, Suites: invocation.Suites, RuntimeContext: "free"},
		{Phases: invocation.Phases, Suites: []string{"unit"}, RuntimeContext: "free"},
		{Phases: invocation.Phases, Suites: invocation.Suites, RuntimeContext: "container"},
	} {
		if _, err := readReplayPlan(ctx, workspace, file, &options, changed); err == nil {
			t.Fatalf("accepted changed invocation: %+v", changed)
		}
	}
	actual, err := readReplayPlan(ctx, workspace, file, &options, invocation)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := t.TempDir()
	required := map[string][]string{"accounts": {"organization"}, "gateway": {"organization", "accounts"}, "frontend": {"gateway"}}
	executed := []string{}
	err = CIWithPlanOptions(ctx, workspace, actual, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		for _, name := range required[service.Name] {
			if _, err := os.Stat(filepath.Join(artifacts, name)); err != nil {
				return err
			}
		}
		executed = append(executed, service.Name)
		return os.WriteFile(filepath.Join(artifacts, service.Name), []byte("fresh"), 0o600)
	}, ScheduleOptions{Phase: "build", Jobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(executed, []string{"organization", "accounts", "gateway", "frontend"}) {
		t.Fatalf("executed %v", executed)
	}
	saved.Tasks[0].Prerequisites = nil
	data, err = json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReplayPlan(ctx, workspace, file, &options, invocation); err == nil {
		t.Fatal("accepted removed task prerequisites")
	}
}

func TestReplaySnapshotExcludesOwnedReportsButIncludesSource(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	before, err := replayInputs(context.Background(), workspace, root)
	if err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, filepath.Join(root, ".codefly/ci/report.json"), `{"status":"passed"}`)
	after, err := replayInputs(context.Background(), workspace, root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("owned report output invalidated source snapshot")
	}
	writeCacheTestFile(t, filepath.Join(root, "modules/web/services/frontend/code/ignored.input"), "changed")
	changed, err := replayInputs(context.Background(), workspace, root)
	if err != nil {
		t.Fatal(err)
	}
	if changed == after {
		t.Fatal("source change was excluded")
	}
	runCacheTestGit(t, root, "add", ".codefly/ci/report.json")
	tracked, err := replayInputs(context.Background(), workspace, root)
	if err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, filepath.Join(root, ".codefly/ci/report.json"), `{"input":"changed"}`)
	changed, err = replayInputs(context.Background(), workspace, root)
	if err != nil {
		t.Fatal(err)
	}
	if changed == tracked {
		t.Fatal("tracked report input was excluded")
	}
}

func TestReplayRejectsUndispatchableSchemaJobs(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	modulePath := filepath.Join(root, "modules/management/module.codefly.yaml")
	payload, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte("\njobs:\n - name: prepare\n")...)
	if err := os.WriteFile(modulePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, filepath.Join(root, "modules/management/jobs/prepare/job.codefly.yaml"), `kind: job
name: prepare
version: 0.0.1
execution:
 type: one-shot
 timeout: 5m
agent:
 kind: codefly:job
 name: go-job
 publisher: codefly.ai
 version: 0.0.1
service-dependencies:
 - name: organization
   module: management
`)
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{All: true, ChangedFiles: []string{"README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildReplayPlan(context.Background(), workspace, plan, ReplayInvocation{}); err == nil || !strings.Contains(err.Error(), "no executor for schema job management/prepare") {
		t.Fatalf("schema jobs: %v", err)
	}
}
