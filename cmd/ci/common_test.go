package ci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
)

func TestCIWithPlanRunsOnlyPlannedServicesInPlanOrder(t *testing.T) {
	_, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	plan := &Plan{Services: []PlannedService{
		{Service: "management/organization", Classification: "direct"},
		{Service: "web/frontend", Classification: "dependent"},
	}}
	var got []string
	err := CIWithPlan(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		got = append(got, resources.WithUnique(service).Unique())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"management/organization", "web/frontend"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("executed services = %v, want %v", got, want)
	}
}

func TestCIWithPlanOptionsPreservesTransitivePrerequisiteOrder(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{
		{Service: "web/frontend"},
		{Service: "management/organization"},
	}}
	var organizationDone atomic.Bool
	err := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		switch resources.WithUnique(service).Unique() {
		case "management/organization":
			time.Sleep(20 * time.Millisecond)
			organizationDone.Store(true)
		case "web/frontend":
			if !organizationDone.Load() {
				return errors.New("frontend ran before its transitive prerequisite")
			}
		}
		return nil
	}, ScheduleOptions{Jobs: 2, FailFast: true})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCIWithPlanOptionsRunsIndependentTargetsConcurrently(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/worker"}, {Service: "web/frontend"}}}
	entered := make(chan string, 2)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
			entered <- resources.WithUnique(service).Unique()
			<-release
			return nil
		}, ScheduleOptions{Jobs: 2, FailFast: true})
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("independent service tasks did not overlap")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCIWithPlanOptionsSerializesSharedRuntimeClosure(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/consumer"}, {Service: "web/frontend"}}}
	entered := make(chan string, 2)
	release := make(chan struct{})
	done := make(chan error, 1)
	var active atomic.Int32
	var maximum atomic.Int32
	go func() {
		done <- CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
			current := active.Add(1)
			for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
			}
			entered <- resources.WithUnique(service).Unique()
			<-release
			active.Add(-1)
			return nil
		}, ScheduleOptions{Jobs: 2, FailFast: true, LockDependencyClosure: true})
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first runtime task did not start")
	}
	select {
	case second := <-entered:
		close(release)
		t.Fatalf("shared dependency closure overlapped at %s", second)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second runtime task did not start after resource release")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent runtime closures = %d, want 1", maximum.Load())
	}
}

func TestCIWithPlanOptionsContinuesIndependentWorkAndBlocksDependents(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{
		{Service: "management/organization"},
		{Service: "billing/accounts"},
		{Service: "management/worker"},
	}}
	var ranAccounts atomic.Bool
	var ranWorker atomic.Bool
	err := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		switch resources.WithUnique(service).Unique() {
		case "management/organization":
			return errors.New("organization failed")
		case "billing/accounts":
			ranAccounts.Store(true)
		case "management/worker":
			ranWorker.Store(true)
		}
		return nil
	}, ScheduleOptions{Jobs: 2, FailFast: false})
	if err == nil || !strings.Contains(err.Error(), "organization failed") {
		t.Fatalf("error = %v, want organization failure", err)
	}
	if ranAccounts.Load() {
		t.Fatal("dependent task ran after its prerequisite failed")
	}
	if !ranWorker.Load() {
		t.Fatal("independent task did not run with fail-fast disabled")
	}
}

func TestCIWithPlanOptionsFailFastStopsUnscheduledWork(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "management/worker"}}}
	var workerRan atomic.Bool
	err := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		if resources.WithUnique(service).Unique() == "management/organization" {
			return errors.New("stop now")
		}
		workerRan.Store(true)
		return nil
	}, ScheduleOptions{Jobs: 1, FailFast: true})
	if err == nil || workerRan.Load() {
		t.Fatalf("fail-fast result: error=%v worker_ran=%v", err, workerRan.Load())
	}
}

func TestCIWithPlanOptionsReportsConcurrentFailuresInPlanOrder(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/consumer"}, {Service: "management/worker"}}}
	err := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		unique := resources.WithUnique(service).Unique()
		if unique == "management/consumer" {
			time.Sleep(20 * time.Millisecond)
		}
		return errors.New("failure from " + unique)
	}, ScheduleOptions{Jobs: 2, FailFast: false})
	if err == nil {
		t.Fatal("concurrent failures unexpectedly passed")
	}
	message := err.Error()
	consumer := strings.Index(message, "service management/consumer")
	worker := strings.Index(message, "service management/worker")
	if consumer < 0 || worker < 0 || consumer > worker {
		t.Fatalf("failure order is not plan-deterministic: %s", message)
	}
}

func TestParsePortOverrides(t *testing.T) {
	overrides, err := parsePortOverrides([]string{"app/subject/rest=45001", " app/subject/grpc = 45003 "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := overrides["app/subject/rest"]; got != 45001 {
		t.Fatalf("rest port = %d, want 45001", got)
	}
	if got := overrides["app/subject/grpc"]; got != 45003 {
		t.Fatalf("grpc port = %d, want 45003 (surrounding spaces trimmed)", got)
	}

	if got, err := parsePortOverrides(nil); got != nil || err != nil {
		t.Fatalf("empty input = (%v, %v), want (nil, nil)", got, err)
	}

	for _, bad := range []string{"app/subject/rest", "app/subject/rest=", "=45001", "app/subject/rest=0", "app/subject/rest=70000", "app/subject/rest=abc"} {
		if _, err := parsePortOverrides([]string{bad}); err == nil {
			t.Fatalf("parsePortOverrides(%q) = nil error, want a rejection", bad)
		}
	}
}

func TestNormalizeCIJobs(t *testing.T) {
	if jobs, err := normalizeCIJobs(2); err != nil || jobs != 2 {
		t.Fatalf("explicit jobs = %d, %v", jobs, err)
	}
	if jobs, err := normalizeCIJobs(0); err != nil || jobs < 1 || jobs > 4 {
		t.Fatalf("automatic jobs = %d, %v", jobs, err)
	}
	if _, err := normalizeCIJobs(-1); err == nil {
		t.Fatal("negative jobs value was accepted")
	}
}

func TestCIWithPlanOptionsDrainsRunningTasksOnCancellation(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/worker"}, {Service: "web/frontend"}}}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	var finished atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- CIWithPlanOptions(ctx, workspace, plan, func(ctx context.Context, _ *resources.Workspace, _ *resources.Module, _ *resources.Service) error {
			started <- struct{}{}
			<-ctx.Done()
			finished.Add(1)
			return ctx.Err()
		}, ScheduleOptions{Jobs: 2, FailFast: false})
	}()
	<-started
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not drain cancelled tasks")
	}
	if finished.Load() != 2 {
		t.Fatalf("finished tasks = %d, want 2", finished.Load())
	}
}

func loadSchedulerFixture(t testing.TB) (string, *resources.Workspace) {
	t.Helper()
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != resources.ServiceConfigurationName {
			return nil
		}
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(path, []byte(strings.ReplaceAll(strings.ReplaceAll(string(payload), "visibility: application", "visibility: public"), "name: grpc\n      api:", "name: grpc\n      visibility: public\n      api:")), 0o600)
	}); err != nil {
		t.Fatal(err)
	}

	modulePath := filepath.Join(root, "modules", "management", "module.codefly.yaml")
	module, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	module = append(module, []byte("\n    - name: worker\n    - name: consumer\n")...)
	if err := os.WriteFile(modulePath, module, 0o644); err != nil {
		t.Fatal(err)
	}
	serviceDir := filepath.Join(root, "modules", "management", "services", "worker")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	service := []byte(`kind: service
name: worker
module: management
version: 0.0.0
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
`)
	if err := os.WriteFile(filepath.Join(serviceDir, "service.codefly.yaml"), service, 0o644); err != nil {
		t.Fatal(err)
	}
	consumerDir := filepath.Join(root, "modules", "management", "services", "consumer")
	if err := os.MkdirAll(consumerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	consumer := []byte(`kind: service
name: consumer
module: management
version: 0.0.0
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
service-dependencies:
    - name: organization
      module: management
`)
	if err := os.WriteFile(filepath.Join(consumerDir, "service.codefly.yaml"), consumer, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, workspace
}

func TestCIBuildPreservesPrerequisites(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, jobs := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("selected=%t/jobs=%d", selected, jobs), func(t *testing.T) {
				_, workspace := loadSchedulerFixture(t)
				plan, err := BuildPlan(context.Background(), workspace, PlanOptions{All: true})
				if err != nil {
					t.Fatal(err)
				}
				required := map[string][]string{
					"billing/accounts":    {"management/organization"},
					"management/consumer": {"management/organization"},
					"web/gateway":         {"management/organization", "billing/accounts"},
					"web/frontend":        {"web/gateway"},
				}
				if selected {
					plan.Services = []PlannedService{{Service: "web/frontend"}, {Service: "management/organization"}}
					required["web/frontend"] = []string{"management/organization", "billing/accounts", "web/gateway"}
				}
				var mu sync.Mutex
				completed := map[string]bool{}
				err = CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
					unique := resources.WithUnique(service).Unique()
					mu.Lock()
					defer mu.Unlock()
					for _, prerequisite := range required[unique] {
						if !completed[prerequisite] {
							return fmt.Errorf("%s ran before %s", unique, prerequisite)
						}
					}
					if completed[unique] {
						return fmt.Errorf("%s ran twice", unique)
					}
					completed[unique] = true
					return nil
				}, ScheduleOptions{Jobs: jobs, Phase: "build"})
				if err != nil {
					t.Fatal(err)
				}
				wantCount := len(plan.Services)
				if selected {
					wantCount = 4
				}
				if len(completed) != wantCount {
					t.Fatalf("completed %d of %d tasks", len(completed), wantCount)
				}
			})
		}
	}
}

func TestCIBuildFailureBlocksConsumersAndPreservesIndependentWork(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "web/frontend"}, {Service: "management/worker"}}}
	reporter := fixedCIReporter(t, plan)
	options := ScheduleOptions{Jobs: 1, Phase: "build", Reporter: reporter}
	if err := prepareCIReportTasks(context.Background(), workspace, plan, options); err != nil {
		t.Fatal(err)
	}
	err := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		if service.Name == "organization" {
			return errors.New("image build failed")
		}
		return nil
	}, options)
	if err == nil || !strings.Contains(err.Error(), "image build failed") {
		t.Fatalf("build failure lost: %v", err)
	}
	report := reporter.Finalize(err)
	if report.Status != reportStatusFailed {
		t.Fatalf("gate status = %s", report.Status)
	}
	assertReportTask(t, report.Tasks[0], reportStatusFailed, "")
	assertReportTask(t, report.Tasks[1], reportStatusSkipped, reportReasonFailedPrerequisite)
	assertReportTask(t, report.Tasks[2], reportStatusPassed, "")
	if !reflect.DeepEqual(report.Tasks[1].Prerequisites, []string{"billing/accounts", "management/organization", "web/gateway"}) || !reflect.DeepEqual(report.Tasks[1].BlockedBy, []string{"management/organization"}) {
		t.Fatalf("consumer prerequisite evidence = %+v", report.Tasks[1])
	}
	for _, task := range report.Tasks {
		if !reflect.DeepEqual(task.RuntimeResources, []string{task.Service}) {
			t.Fatalf("standalone build has runtime dependencies: %+v", task)
		}
	}
}

func TestCIBuildRejectsRuntimeCycle(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	path := filepath.Join(root, "modules/management/services/organization/service.codefly.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, []byte("\nservice-dependencies:\n  - name: frontend\n    module: web\n")...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "web/frontend"}}}
	_, err = buildScheduledTasks(context.Background(), workspace, plan, ScheduleOptions{Phase: "build"})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected topology cycle rejection, got %v", err)
	}
}

func TestCICancelledPlanDoesNotStartTasks(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}}}
	var ran atomic.Bool
	err := CIWithPlanOptions(ctx, workspace, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
		ran.Store(true)
		return nil
	}, ScheduleOptions{Jobs: 1, Phase: "build"})
	if !errors.Is(err, context.Canceled) || ran.Load() {
		t.Fatalf("cancelled scheduling: error=%v ran=%t", err, ran.Load())
	}
}

func TestCISchedulerRetainsConservativePrerequisites(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "web/frontend"}}}
	for _, options := range []ScheduleOptions{
		{},
		{Phase: "unknown"},
		{Phase: "lint"},
		{Phase: "compile"},
		{Phase: "build"},
		{Phase: "test", LockDependencyClosure: true},
		{Phase: "build", LockDependencyClosure: true},
	} {
		t.Run(options.Phase, func(t *testing.T) {
			tasks, err := buildScheduledTasks(context.Background(), workspace, plan, options)
			if err != nil {
				t.Fatal(err)
			}
			wantPrerequisites := []string{"management/organization"}
			if options.Phase == "build" {
				wantPrerequisites = []string{"billing/accounts", "management/organization", "web/gateway"}
			}
			if !reflect.DeepEqual(tasks[1].prerequisites, wantPrerequisites) {
				t.Fatalf("prerequisites = %v", tasks[1].prerequisites)
			}
			if options.LockDependencyClosure && !reflect.DeepEqual(tasks[1].resources, []string{
				"billing/accounts", "management/organization", "web/frontend", "web/gateway",
			}) {
				t.Fatalf("runtime resources = %v", tasks[1].resources)
			}
		})
	}
}

func TestCIStageGraphs(t *testing.T) {
	for _, kind := range []string{"runtime", "completion", "build", "schema", ""} {
		t.Run(kind, func(t *testing.T) {
			root, workspace := loadSchedulerFixture(t)
			path := filepath.Join(root, "modules/web/services/frontend/service.codefly.yaml")
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "" {
				payload = []byte(strings.Replace(string(payload), "- name: gateway", "- kind: "+kind+"\n      name: gateway", 1))
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "web/frontend"}}}
			for _, phase := range []string{"build", "test"} {
				tasks, err := buildScheduledTasks(context.Background(), workspace, plan, ScheduleOptions{Phase: phase, LockDependencyClosure: phase == "test"})
				if err != nil {
					t.Fatal(err)
				}
				wantsEdge := kind == "" || (phase == "build" && (kind == "build" || kind == "schema")) || (phase == "test" && (kind == "runtime" || kind == "completion"))
				if phase == "test" && len(tasks[1].resources) != 4 {
					t.Fatalf("runtime ownership was weakened: %v", tasks[1].resources)
				}
				if (len(tasks[1].prerequisites) > 0) != wantsEdge {
					t.Fatalf("%s %s prerequisites: %v", kind, phase, tasks[1].prerequisites)
				}
			}
		})
	}
}

func TestCIMixedStageCycleCanPlanAndSchedule(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	for _, entry := range []struct{ path, content string }{
		{"modules/management/services/organization/service.codefly.yaml", "\nservice-dependencies:\n - name: frontend\n   module: web\n   kind: build\n"},
	} {
		path := filepath.Join(root, entry.path)
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(payload, []byte(entry.content)...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "modules/web/services/frontend/service.codefly.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(strings.Replace(string(payload), "- name: gateway", "- kind: runtime\n      name: gateway", 1))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), workspace, PlanOptions{All: true, ChangedFiles: []string{"README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	replay, err := buildReplayPlan(context.Background(), workspace, plan, ReplayInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	stages := map[resources.Stage]bool{}
	for _, task := range replay.Tasks {
		stages[task.Stage] = true
	}
	if !stages[resources.StageBuild] || !stages[resources.StageRun] {
		t.Fatal("missing executable stages")
	}
	for _, phase := range []string{"build", "test"} {
		if _, err := buildScheduledTasks(context.Background(), workspace, plan, ScheduleOptions{Phase: phase, LockDependencyClosure: phase == "test"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCIReportsAllFailedPrerequisites(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	path := filepath.Join(root, "modules/web/services/frontend/service.codefly.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(strings.Replace(string(payload), "service-dependencies:", "service-dependencies:\n    - name: worker\n      module: management", 1))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "management/worker"}, {Service: "web/frontend"}}}
	reporter := fixedCIReporter(t, plan)
	err = CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		if service.Name == "frontend" {
			t.Error("blocked task executed")
		}
		return fmt.Errorf("failed %s", service.Name)
	}, ScheduleOptions{Phase: "build", Jobs: 2, Reporter: reporter})
	if err == nil {
		t.Fatal("required failures did not fail gate")
	}
	report := reporter.Finalize(err)
	if report.Tasks[2].Stage != resources.StageBuild {
		t.Fatalf("missing report stage: %+v", report.Tasks[2])
	}
	if !reflect.DeepEqual(report.Tasks[2].BlockedBy, []string{"management/organization", "management/worker"}) {
		t.Fatalf("blockers: %v", report.Tasks[2].BlockedBy)
	}
}

func BenchmarkCIStageScheduling(b *testing.B) {
	for _, phase := range []string{"build", "test"} {
		for _, selected := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/selected=%t", phase, selected), func(b *testing.B) {
				_, workspace := loadSchedulerFixture(b)
				plan, err := BuildPlan(context.Background(), workspace, PlanOptions{All: true, ChangedFiles: []string{"README.md"}})
				if err != nil {
					b.Fatal(err)
				}
				if selected {
					plan.Services = []PlannedService{{Service: "management/organization"}, {Service: "web/frontend"}}
				}
				options := ScheduleOptions{Phase: phase, LockDependencyClosure: phase == "test"}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, err := buildScheduledTasks(context.Background(), workspace, plan, options); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
