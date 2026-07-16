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

func TestCIReportPreservesPlanOrderAndDependencyIdentity(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{
		SchemaVersion: planSchemaVersion,
		Workspace:     workspace.Name,
		Services: []PlannedService{
			{Service: "web/frontend", Classification: "dependent", Reasons: []string{"upstream contract changed"}},
			{Service: "management/organization", Classification: "direct", Reasons: []string{"service input changed"}},
		},
		ChangedFiles: []string{"modules/management/services/organization/api.proto"},
	}
	reporter := fixedCIReporter(t, plan)
	var prerequisiteDone atomic.Bool
	err := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		switch resources.WithUnique(service).Unique() {
		case "management/organization":
			prerequisiteDone.Store(true)
		case "web/frontend":
			if !prerequisiteDone.Load() {
				return errors.New("frontend ran before organization")
			}
		}
		return nil
	}, ScheduleOptions{Jobs: 2, FailFast: true, Phase: "compile", Reporter: reporter})
	if err != nil {
		t.Fatal(err)
	}

	report := reporter.Finalize(nil)
	if report.Status != reportStatusPassed {
		t.Fatalf("report status = %q, want passed", report.Status)
	}
	if got, want := report.Summary, (CIReportSummary{Total: 2, Passed: 2}); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}
	if got := []string{report.Tasks[0].Service, report.Tasks[1].Service}; !reflect.DeepEqual(got, []string{"web/frontend", "management/organization"}) {
		t.Fatalf("task order = %v", got)
	}
	frontend := report.Tasks[0]
	if frontend.ID != "compile:web/frontend" {
		t.Fatalf("task id = %q", frontend.ID)
	}
	if !reflect.DeepEqual(frontend.Prerequisites, []string{"management/organization"}) {
		t.Fatalf("frontend prerequisites = %v", frontend.Prerequisites)
	}
	if !reflect.DeepEqual(frontend.RuntimeResources, []string{"web/frontend"}) {
		t.Fatalf("frontend runtime resources = %v", frontend.RuntimeResources)
	}
}

func TestCIReportRecordsFailedPrerequisiteAndIndependentSuccess(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{SchemaVersion: planSchemaVersion, Workspace: workspace.Name, ChangedFiles: []string{}, Services: []PlannedService{
		{Service: "management/organization"},
		{Service: "billing/accounts"},
		{Service: "management/worker"},
	}}
	reporter := fixedCIReporter(t, plan)
	runErr := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
		if resources.WithUnique(service).Unique() == "management/organization" {
			return errors.New("organization validation failed")
		}
		return nil
	}, ScheduleOptions{Jobs: 2, FailFast: false, Phase: "lint", Reporter: reporter})
	if runErr == nil {
		t.Fatal("CI unexpectedly passed")
	}

	report := reporter.Finalize(runErr)
	if report.Status != reportStatusFailed {
		t.Fatalf("report status = %q, want failed", report.Status)
	}
	assertReportTask(t, report.Tasks[0], reportStatusFailed, "")
	assertReportTask(t, report.Tasks[1], reportStatusSkipped, reportReasonFailedPrerequisite)
	if !reflect.DeepEqual(report.Tasks[1].BlockedBy, []string{"management/organization"}) {
		t.Fatalf("accounts blocked_by = %v", report.Tasks[1].BlockedBy)
	}
	assertReportTask(t, report.Tasks[2], reportStatusPassed, "")
	if got, want := report.Summary, (CIReportSummary{Total: 3, Passed: 1, Failed: 1, Skipped: 1}); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}
}

func TestCIReportIncludesLaterPhasesAfterFailFast(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{SchemaVersion: planSchemaVersion, Workspace: workspace.Name, ChangedFiles: []string{}, Services: []PlannedService{
		{Service: "management/organization"},
		{Service: "management/worker"},
	}}
	reporter := fixedCIReporter(t, plan)
	lint := ScheduleOptions{Jobs: 1, FailFast: true, Phase: "lint", Reporter: reporter}
	compile := ScheduleOptions{Jobs: 1, FailFast: true, Phase: "compile", Reporter: reporter}
	testSuite := ScheduleOptions{Jobs: 1, FailFast: true, LockDependencyClosure: true, Phase: "test", Suite: "unit", Reporter: reporter}
	for _, options := range []ScheduleOptions{lint, compile, testSuite} {
		if err := prepareCIReportTasks(context.Background(), workspace, plan, options); err != nil {
			t.Fatal(err)
		}
	}

	runErr := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, _ *resources.Service) error {
		return errors.New("lint failed")
	}, lint)
	if runErr == nil {
		t.Fatal("lint unexpectedly passed")
	}
	report := reporter.Finalize(runErr)
	if !reflect.DeepEqual(report.Phases, []string{"lint", "compile", "test"}) {
		t.Fatalf("phases = %v", report.Phases)
	}
	if got := len(report.Tasks); got != 6 {
		t.Fatalf("task count = %d, want 6", got)
	}
	assertReportTask(t, report.Tasks[0], reportStatusFailed, "")
	assertReportTask(t, report.Tasks[1], reportStatusSkipped, reportReasonFailFast)
	for _, task := range report.Tasks[2:] {
		assertReportTask(t, task, reportStatusSkipped, reportReasonFailFast)
	}
	if report.Tasks[4].ID != "test:unit:management/organization" || report.Tasks[4].Suite != "unit" {
		t.Fatalf("suite task = %#v", report.Tasks[4])
	}
}

func TestCIReportMarksRunningAndPendingTasksCancelled(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{SchemaVersion: planSchemaVersion, Workspace: workspace.Name, ChangedFiles: []string{}, Services: []PlannedService{
		{Service: "management/worker"},
		{Service: "web/frontend"},
	}}
	reporter := fixedCIReporter(t, plan)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- CIWithPlanOptions(ctx, workspace, plan, func(ctx context.Context, _ *resources.Workspace, _ *resources.Module, _ *resources.Service) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}, ScheduleOptions{Jobs: 1, FailFast: false, Phase: "test", Reporter: reporter})
	}()
	<-started
	cancel()
	runErr := <-done
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("run error = %v, want cancellation", runErr)
	}

	report := reporter.Finalize(runErr)
	if report.Status != reportStatusCancelled {
		t.Fatalf("report status = %q, want cancelled", report.Status)
	}
	for _, task := range report.Tasks {
		assertReportTask(t, task, reportStatusCancelled, reportReasonRunCancelled)
	}
	if got, want := report.Summary, (CIReportSummary{Total: 2, Cancelled: 2}); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}
}

func TestCIReportJSONIsStableAndWrittenRelativeToWorkspace(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{SchemaVersion: planSchemaVersion, Workspace: workspace.Name, ChangedFiles: []string{}, Services: []PlannedService{
		{Service: "management/worker", Reasons: []string{}},
	}}
	reporter := fixedCIReporter(t, plan)
	err := CIWithPlanOptions(context.Background(), workspace, plan, func(_ context.Context, _ *resources.Workspace, _ *resources.Module, _ *resources.Service) error {
		return nil
	}, ScheduleOptions{Jobs: 1, FailFast: true, Phase: "test", Reporter: reporter})
	if err != nil {
		t.Fatal(err)
	}
	report := reporter.Finalize(nil)
	first, err := marshalCIReport(report)
	if err != nil {
		t.Fatal(err)
	}
	second, err := marshalCIReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("repeated report marshaling was not deterministic")
	}
	if strings.Contains(string(first), `"selection_reasons": null`) || strings.Contains(string(first), `"prerequisites": null`) {
		t.Fatalf("required report arrays encoded as null:\n%s", first)
	}
	if !strings.Contains(string(first), `"id": "test:default:management/worker"`) {
		t.Fatalf("default suite identity missing:\n%s", first)
	}

	destination, written, err := writeCIReport(workspace, ".codefly/test-report", report)
	if err != nil {
		t.Fatal(err)
	}
	wantDestination := filepath.Join(workspace.Dir(), ".codefly", "test-report", reportFilename)
	if destination != wantDestination {
		t.Fatalf("destination = %q, want %q", destination, wantDestination)
	}
	onDisk, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(written) || string(written) != string(first) {
		t.Fatal("atomic report artifact differs from canonical JSON")
	}
}

func TestCIReportRecordsWorkspaceTaskAndTypedEvidence(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{SchemaVersion: planSchemaVersion, Workspace: workspace.Name, ChangedFiles: []string{}, Services: []PlannedService{
		{Service: "management/worker"},
	}}
	reporter := fixedCIReporter(t, plan)
	if err := runReportedWorkspacePhase(context.Background(), reporter, workspace, "verify", func(ctx context.Context) error {
		recordCIReportIntegrity(ctx, CIReportIntegrity{GuardedModules: 1, Modules: []CIReportIntegrityModule{{Module: "management", Omitted: map[string]int{}, Allowed: []CIReportIntegrityDivergence{}, Missing: []string{}, Modified: []string{}}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	options := ScheduleOptions{Jobs: 1, FailFast: true, Phase: "audit", Reporter: reporter}
	if err := prepareCIReportTasks(context.Background(), workspace, plan, options); err != nil {
		t.Fatal(err)
	}
	id := reportTaskID("audit", "", "management/worker")
	reporter.startTask(id)
	ctx := withCIReportTask(context.Background(), reporter, id)
	recordCIReportAudit(ctx, CIReportAudit{State: "FINDINGS", Tool: "scanner", Findings: 2, High: 1})
	recordCIReportDrift(ctx, []string{"b.ts", "a.ts"})
	recordCIReportArtifact(ctx, CIReportArtifact{Kind: "cyclonedx-sbom", Path: "sbom/worker.cdx.json", SHA256: "sha256:abc"})
	reporter.finishTask(id, nil)

	report := reporter.Finalize(nil)
	if got, want := report.Phases, []string{"verify", "audit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	workspaceTask := report.Tasks[0]
	if workspaceTask.ID != "verify:workspace" || workspaceTask.Scope != "workspace" || workspaceTask.Resource != workspace.Name || workspaceTask.Service != "" {
		t.Fatalf("workspace task = %#v", workspaceTask)
	}
	if workspaceTask.Integrity == nil || workspaceTask.Integrity.GuardedModules != 1 {
		t.Fatalf("workspace integrity evidence = %#v", workspaceTask.Integrity)
	}
	serviceTask := report.Tasks[1]
	if serviceTask.Scope != "service" || serviceTask.Resource != "management/worker" {
		t.Fatalf("service task scope = %#v", serviceTask)
	}
	if serviceTask.Audit == nil || serviceTask.Audit.High != 1 || serviceTask.Drift == nil || len(serviceTask.Artifacts) != 1 {
		t.Fatalf("typed task evidence = %#v", serviceTask)
	}
	serviceTask.Drift.ChangedFiles[0] = "mutated"
	if reporter.report.Tasks[1].Drift.ChangedFiles[0] == "mutated" {
		t.Fatal("finalized report shares drift evidence with reporter state")
	}
}

func fixedCIReporter(t *testing.T, plan *Plan) *CIReporter {
	t.Helper()
	fixed := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	reporter, err := newCIReporter(plan, "codefly ci run", "test-version", func() time.Time { return fixed })
	if err != nil {
		t.Fatal(err)
	}
	return reporter
}

func assertReportTask(t *testing.T, task CIReportTask, status, reason string) {
	t.Helper()
	if task.Status != status || task.StatusReason != reason {
		t.Fatalf("task %s outcome = %s/%s, want %s/%s", task.ID, task.Status, task.StatusReason, status, reason)
	}
}
