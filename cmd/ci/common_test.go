package ci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func loadSchedulerFixture(t *testing.T) (string, *resources.Workspace) {
	t.Helper()
	root, workspace := loadPlanFixture(t, "../../pkg/orchestration/testdata/module-layout")
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
