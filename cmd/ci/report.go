package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

const (
	reportSchemaVersion = 1
	reportFilename      = "report.json"

	reportStatusPending   = "pending"
	reportStatusRunning   = "running"
	reportStatusPassed    = "passed"
	reportStatusFailed    = "failed"
	reportStatusSkipped   = "skipped"
	reportStatusCancelled = "cancelled"

	reportReasonFailedPrerequisite    = "failed_prerequisite"
	reportReasonFailFast              = "fail_fast"
	reportReasonRunCancelled          = "run_cancelled"
	reportReasonNotScheduled          = "not_scheduled"
	reportReasonAgentNoSyncCapability = "agent_no_sync_capability"
)

// CIReport is Codefly's provider-neutral record of one CI command. Task order
// is invocation order, then affected-service plan order; it never depends on
// concurrent completion order.
type CIReport struct {
	SchemaVersion  int             `json:"schema_version"`
	Command        string          `json:"command"`
	CodeflyVersion string          `json:"codefly_version"`
	Plan           Plan            `json:"plan"`
	Phases         []string        `json:"phases"`
	Status         string          `json:"status"`
	StartedAt      string          `json:"started_at"`
	FinishedAt     string          `json:"finished_at"`
	DurationMS     int64           `json:"duration_ms"`
	Summary        CIReportSummary `json:"summary"`
	Tasks          []CIReportTask  `json:"tasks"`
	Error          string          `json:"error,omitempty"`
}

type CIReportSummary struct {
	Total     int `json:"total"`
	Passed    int `json:"passed"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Cancelled int `json:"cancelled"`
}

// CIReportTask describes the logical operation even when it never executes.
// ID is stable for a given phase, optional suite, and service; a future cache
// content digest can therefore attach to the task without changing identity.
type CIReportTask struct {
	ID               string             `json:"id"`
	Scope            string             `json:"scope"`
	Resource         string             `json:"resource"`
	Phase            string             `json:"phase"`
	Suite            string             `json:"suite,omitempty"`
	Service          string             `json:"service,omitempty"`
	Classification   string             `json:"classification,omitempty"`
	SelectionReasons []string           `json:"selection_reasons"`
	Prerequisites    []string           `json:"prerequisites"`
	RuntimeResources []string           `json:"runtime_resources"`
	Cache            CICacheIdentity    `json:"cache"`
	Audit            *CIReportAudit     `json:"audit,omitempty"`
	Drift            *CIReportDrift     `json:"drift,omitempty"`
	Integrity        *CIReportIntegrity `json:"integrity,omitempty"`
	Artifacts        []CIReportArtifact `json:"artifacts,omitempty"`
	Status           string             `json:"status"`
	StatusReason     string             `json:"status_reason,omitempty"`
	BlockedBy        []string           `json:"blocked_by,omitempty"`
	StartedAt        string             `json:"started_at,omitempty"`
	FinishedAt       string             `json:"finished_at,omitempty"`
	DurationMS       int64              `json:"duration_ms"`
	Error            string             `json:"error,omitempty"`
}

type CIReportAudit struct {
	State    string `json:"state"`
	Tool     string `json:"tool,omitempty"`
	Language string `json:"language,omitempty"`
	Findings int    `json:"findings"`
	Low      int    `json:"low"`
	Medium   int    `json:"medium"`
	High     int    `json:"high"`
	Critical int    `json:"critical"`
	Outdated int    `json:"outdated"`
}

type CIReportDrift struct {
	ChangedFiles []string `json:"changed_files"`
}

type CIReportIntegrity struct {
	GuardedModules int                       `json:"guarded_modules"`
	FailedModules  int                       `json:"failed_modules"`
	Modules        []CIReportIntegrityModule `json:"modules"`
}

type CIReportIntegrityModule struct {
	Module   string                        `json:"module"`
	Files    int                           `json:"files"`
	Omitted  map[string]int                `json:"omitted"`
	Allowed  []CIReportIntegrityDivergence `json:"allowed"`
	Missing  []string                      `json:"missing"`
	Modified []string                      `json:"modified"`
	Error    string                        `json:"error,omitempty"`
}

type CIReportIntegrityDivergence struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type CIReportArtifact struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	MediaType string `json:"media_type,omitempty"`
	SHA256    string `json:"sha256"`
}

type ciReportTaskContextKey struct{}

type machineReadableCIError struct {
	error
}

func (machineReadableCIError) MachineReadable() bool { return true }

type ciReportTaskContext struct {
	reporter *CIReporter
	id       string
}

func withCIReportTask(ctx context.Context, reporter *CIReporter, id string) context.Context {
	if reporter == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, ciReportTaskContextKey{}, ciReportTaskContext{reporter: reporter, id: id})
}

func recordCIReportAudit(ctx context.Context, audit CIReportAudit) {
	task, ok := ctx.Value(ciReportTaskContextKey{}).(ciReportTaskContext)
	if !ok || task.reporter == nil {
		return
	}
	task.reporter.mu.Lock()
	defer task.reporter.mu.Unlock()
	if reportTask, found := task.reporter.task(task.id); found {
		copy := audit
		reportTask.Audit = &copy
	}
}

func recordCIReportDrift(ctx context.Context, changed []string) {
	task, ok := ctx.Value(ciReportTaskContextKey{}).(ciReportTaskContext)
	if !ok || task.reporter == nil {
		return
	}
	task.reporter.mu.Lock()
	defer task.reporter.mu.Unlock()
	if reportTask, found := task.reporter.task(task.id); found {
		reportTask.Drift = &CIReportDrift{ChangedFiles: cloneStrings(changed)}
	}
}

// recordCIReportSkip transitions the running task bound to ctx to skipped. A
// phase action calls it when the work is legitimately not applicable (an agent
// that advertises no sync capability owns no generated source to drift), so the
// nil error it returns must read as skipped rather than passed.
func recordCIReportSkip(ctx context.Context, reason string) {
	task, ok := ctx.Value(ciReportTaskContextKey{}).(ciReportTaskContext)
	if !ok || task.reporter == nil {
		return
	}
	task.reporter.markSkipped(task.id, reason)
}

func recordCIReportIntegrity(ctx context.Context, integrity CIReportIntegrity) {
	task, ok := ctx.Value(ciReportTaskContextKey{}).(ciReportTaskContext)
	if !ok || task.reporter == nil {
		return
	}
	task.reporter.mu.Lock()
	defer task.reporter.mu.Unlock()
	if reportTask, found := task.reporter.task(task.id); found {
		copy := cloneCIReportIntegrity(integrity)
		reportTask.Integrity = &copy
	}
}

func recordCIReportArtifact(ctx context.Context, artifact CIReportArtifact) {
	task, ok := ctx.Value(ciReportTaskContextKey{}).(ciReportTaskContext)
	if !ok || task.reporter == nil {
		return
	}
	task.reporter.mu.Lock()
	defer task.reporter.mu.Unlock()
	if reportTask, found := task.reporter.task(task.id); found {
		reportTask.Artifacts = append(reportTask.Artifacts, artifact)
	}
}

type reportClock func() time.Time

type CIReporter struct {
	mu           sync.Mutex
	now          reportClock
	startedAt    time.Time
	report       CIReport
	taskIndex    map[string]int
	cacheBuilder *ciCacheIdentityBuilder
}

func NewCIReporter(plan *Plan, command string) (*CIReporter, error) {
	version, err := cli.GetCurrentVersion()
	if err != nil {
		return nil, fmt.Errorf("read Codefly version for CI report: %w", err)
	}
	return newCIReporter(plan, command, version, time.Now)
}

func newCIReporter(plan *Plan, command, version string, now reportClock) (*CIReporter, error) {
	if plan == nil {
		return nil, fmt.Errorf("CI report plan is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("CI report clock is nil")
	}
	startedAt := now().UTC()
	reporter := &CIReporter{
		now:       now,
		startedAt: startedAt,
		taskIndex: map[string]int{},
		report: CIReport{
			SchemaVersion:  reportSchemaVersion,
			Command:        strings.TrimSpace(command),
			CodeflyVersion: strings.TrimSpace(version),
			Plan:           clonePlan(plan),
			Phases:         []string{},
			Status:         reportStatusRunning,
			StartedAt:      formatReportTime(startedAt),
			Tasks:          []CIReportTask{},
		},
	}
	return reporter, nil
}

func clonePlan(plan *Plan) Plan {
	cloned := *plan
	cloned.ChangedFiles = cloneStrings(plan.ChangedFiles)
	cloned.Services = make([]PlannedService, len(plan.Services))
	for index, service := range plan.Services {
		cloned.Services[index] = service
		cloned.Services[index].Reasons = cloneStrings(service.Reasons)
		cloned.Services[index].Paths = append([]string(nil), service.Paths...)
	}
	return cloned
}

func cloneStrings(values []string) []string {
	return append([]string{}, values...)
}

func formatReportTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func reportTaskID(phase, suite, service string) string {
	phase = strings.TrimSpace(phase)
	suite = strings.TrimSpace(suite)
	if suite == "" {
		if phase == "test" {
			suite = "default"
		} else {
			return phase + ":" + service
		}
	}
	return phase + ":" + suite + ":" + service
}

func (reporter *CIReporter) registerTasks(ctx context.Context, workspace *resources.Workspace, options ScheduleOptions, tasks []ciScheduledTask) ([]string, error) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()

	phase := strings.TrimSpace(options.Phase)
	if phase == "" {
		return nil, fmt.Errorf("CI report phase is empty")
	}
	if !containsString(reporter.report.Phases, phase) {
		reporter.report.Phases = append(reporter.report.Phases, phase)
	}
	if len(tasks) > 0 && reporter.cacheBuilder == nil {
		reporter.cacheBuilder = newCICacheIdentityBuilder(ctx, workspace, reporter.report.CodeflyVersion)
	}

	ids := make([]string, len(tasks))
	for index, task := range tasks {
		id := reportTaskID(phase, options.Suite, task.planned.Service)
		if _, exists := reporter.taskIndex[id]; exists {
			ids[index] = id
			continue
		}
		ids[index] = id
		reporter.taskIndex[id] = len(reporter.report.Tasks)
		cacheIdentity := reporter.cacheBuilder.identity(ctx, options, task.planned)
		reporter.report.Tasks = append(reporter.report.Tasks, CIReportTask{
			ID:               id,
			Scope:            "service",
			Resource:         task.planned.Service,
			Phase:            phase,
			Suite:            strings.TrimSpace(options.Suite),
			Service:          task.planned.Service,
			Classification:   task.planned.Classification,
			SelectionReasons: cloneStrings(task.planned.Reasons),
			Prerequisites:    cloneStrings(task.prerequisites),
			RuntimeResources: cloneStrings(task.resources),
			Cache:            cacheIdentity,
			Status:           reportStatusPending,
		})
	}
	return ids, nil
}

func (reporter *CIReporter) registerWorkspaceTask(ctx context.Context, workspace *resources.Workspace, phase string) (string, error) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return "", fmt.Errorf("CI workspace report phase is empty")
	}
	if workspace == nil {
		return "", fmt.Errorf("CI workspace report task has no workspace")
	}
	if !containsString(reporter.report.Phases, phase) {
		reporter.report.Phases = append(reporter.report.Phases, phase)
	}
	id := phase + ":workspace"
	if _, exists := reporter.taskIndex[id]; exists {
		return id, nil
	}
	if reporter.cacheBuilder == nil {
		reporter.cacheBuilder = newCICacheIdentityBuilder(ctx, workspace, reporter.report.CodeflyVersion)
	}
	options := ScheduleOptions{Phase: phase, RuntimeContext: runtimeContext}
	cacheIdentity := reporter.cacheBuilder.workspaceIdentity(options, workspace.Name)
	reporter.taskIndex[id] = len(reporter.report.Tasks)
	reporter.report.Tasks = append(reporter.report.Tasks, CIReportTask{
		ID:               id,
		Scope:            "workspace",
		Resource:         workspace.Name,
		Phase:            phase,
		SelectionReasons: []string{"workspace invariant"},
		Prerequisites:    []string{},
		RuntimeResources: []string{},
		Cache:            cacheIdentity,
		Status:           reportStatusPending,
	})
	return id, nil
}

func runReportedWorkspacePhase(ctx context.Context, reporter *CIReporter, workspace *resources.Workspace, phase string, action func(context.Context) error) error {
	id, err := reporter.registerWorkspaceTask(ctx, workspace, phase)
	if err != nil {
		return err
	}
	reporter.startTask(id)
	if action != nil {
		err = action(withCIReportTask(ctx, reporter, id))
	}
	reporter.finishTask(id, err)
	return err
}

func prepareCIReportTasks(ctx context.Context, workspace *resources.Workspace, plan *Plan, options ScheduleOptions) error {
	if options.Reporter == nil {
		return nil
	}
	if plan == nil {
		return fmt.Errorf("prepare CI report tasks: plan is nil")
	}
	if len(plan.Services) == 0 {
		_, err := options.Reporter.registerTasks(ctx, workspace, options, nil)
		return err
	}
	tasks, err := buildScheduledTasks(ctx, workspace, plan, options.LockDependencyClosure)
	if err != nil {
		return err
	}
	_, err = options.Reporter.registerTasks(ctx, workspace, options, tasks)
	return err
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (reporter *CIReporter) startTask(id string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	task, ok := reporter.task(id)
	if !ok || task.Status != reportStatusPending {
		return
	}
	now := reporter.now().UTC()
	task.Status = reportStatusRunning
	task.StartedAt = formatReportTime(now)
}

func (reporter *CIReporter) finishTask(id string, err error) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	task, ok := reporter.task(id)
	if !ok || task.Status != reportStatusRunning {
		return
	}
	now := reporter.now().UTC()
	task.FinishedAt = formatReportTime(now)
	task.DurationMS = reportDurationMS(task.StartedAt, now)
	if err == nil {
		task.Status = reportStatusPassed
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		task.Status = reportStatusCancelled
		task.StatusReason = reportReasonRunCancelled
	} else {
		task.Status = reportStatusFailed
	}
	task.Error = err.Error()
}

func reportDurationMS(startedAt string, finishedAt time.Time) int64 {
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return 0
	}
	duration := finishedAt.Sub(started)
	if duration < 0 {
		return 0
	}
	return duration.Milliseconds()
}

// markSkipped downgrades a running task to skipped, stamping completion so the
// subsequent finishTask call (which only acts on running tasks) leaves it
// untouched.
func (reporter *CIReporter) markSkipped(id, reason string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	task, ok := reporter.task(id)
	if !ok || task.Status != reportStatusRunning {
		return
	}
	now := reporter.now().UTC()
	task.FinishedAt = formatReportTime(now)
	task.DurationMS = reportDurationMS(task.StartedAt, now)
	task.Status = reportStatusSkipped
	task.StatusReason = reason
}

func (reporter *CIReporter) skipTask(id, reason string, blockedBy []string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	task, ok := reporter.task(id)
	if !ok || task.Status != reportStatusPending {
		return
	}
	task.Status = reportStatusSkipped
	task.StatusReason = reason
	task.BlockedBy = append([]string(nil), blockedBy...)
}

func (reporter *CIReporter) cancelPendingTask(id string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	task, ok := reporter.task(id)
	if !ok || task.Status != reportStatusPending {
		return
	}
	task.Status = reportStatusCancelled
	task.StatusReason = reportReasonRunCancelled
}

func (reporter *CIReporter) task(id string) (*CIReportTask, bool) {
	index, ok := reporter.taskIndex[id]
	if !ok || index < 0 || index >= len(reporter.report.Tasks) {
		return nil, false
	}
	return &reporter.report.Tasks[index], true
}

func (reporter *CIReporter) Finalize(runErr error) CIReport {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()

	cancelled := errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)
	for index := range reporter.report.Tasks {
		task := &reporter.report.Tasks[index]
		if task.Status != reportStatusPending && task.Status != reportStatusRunning {
			continue
		}
		if cancelled {
			task.Status = reportStatusCancelled
			task.StatusReason = reportReasonRunCancelled
		} else {
			task.Status = reportStatusSkipped
			if runErr != nil {
				task.StatusReason = reportReasonFailFast
			} else {
				task.StatusReason = reportReasonNotScheduled
			}
		}
	}

	finishedAt := reporter.now().UTC()
	reporter.report.FinishedAt = formatReportTime(finishedAt)
	duration := finishedAt.Sub(reporter.startedAt)
	if duration > 0 {
		reporter.report.DurationMS = duration.Milliseconds()
	}
	reporter.report.Summary = summarizeReportTasks(reporter.report.Tasks)
	reporter.report.Status = reportStatusPassed
	if cancelled || reporter.report.Summary.Cancelled > 0 {
		reporter.report.Status = reportStatusCancelled
	} else if runErr != nil || reporter.report.Summary.Failed > 0 {
		reporter.report.Status = reportStatusFailed
	}
	if runErr != nil {
		reporter.report.Error = runErr.Error()
	}
	return cloneCIReport(reporter.report)
}

func summarizeReportTasks(tasks []CIReportTask) CIReportSummary {
	summary := CIReportSummary{Total: len(tasks)}
	for _, task := range tasks {
		switch task.Status {
		case reportStatusPassed:
			summary.Passed++
		case reportStatusFailed:
			summary.Failed++
		case reportStatusSkipped:
			summary.Skipped++
		case reportStatusCancelled:
			summary.Cancelled++
		}
	}
	return summary
}

func cloneCIReport(report CIReport) CIReport {
	cloned := report
	cloned.Plan = clonePlan(&report.Plan)
	cloned.Phases = append([]string(nil), report.Phases...)
	cloned.Tasks = make([]CIReportTask, len(report.Tasks))
	for index, task := range report.Tasks {
		cloned.Tasks[index] = task
		cloned.Tasks[index].SelectionReasons = cloneStrings(task.SelectionReasons)
		cloned.Tasks[index].Prerequisites = cloneStrings(task.Prerequisites)
		cloned.Tasks[index].RuntimeResources = cloneStrings(task.RuntimeResources)
		cloned.Tasks[index].BlockedBy = append([]string(nil), task.BlockedBy...)
		cloned.Tasks[index].Cache.Inputs.Dependencies = append([]CICacheResourceDigest{}, task.Cache.Inputs.Dependencies...)
		cloned.Tasks[index].Cache.Inputs.Libraries = append([]CICacheResourceDigest{}, task.Cache.Inputs.Libraries...)
		cloned.Tasks[index].Cache.Limitations = append([]string(nil), task.Cache.Limitations...)
		if task.Audit != nil {
			audit := *task.Audit
			cloned.Tasks[index].Audit = &audit
		}
		if task.Drift != nil {
			cloned.Tasks[index].Drift = &CIReportDrift{ChangedFiles: cloneStrings(task.Drift.ChangedFiles)}
		}
		if task.Integrity != nil {
			integrity := cloneCIReportIntegrity(*task.Integrity)
			cloned.Tasks[index].Integrity = &integrity
		}
		cloned.Tasks[index].Artifacts = append([]CIReportArtifact(nil), task.Artifacts...)
	}
	return cloned
}

func cloneCIReportIntegrity(integrity CIReportIntegrity) CIReportIntegrity {
	cloned := integrity
	cloned.Modules = make([]CIReportIntegrityModule, len(integrity.Modules))
	for index, module := range integrity.Modules {
		cloned.Modules[index] = module
		cloned.Modules[index].Omitted = make(map[string]int, len(module.Omitted))
		for service, count := range module.Omitted {
			cloned.Modules[index].Omitted[service] = count
		}
		cloned.Modules[index].Allowed = append([]CIReportIntegrityDivergence{}, module.Allowed...)
		cloned.Modules[index].Missing = cloneStrings(module.Missing)
		cloned.Modules[index].Modified = cloneStrings(module.Modified)
	}
	return cloned
}

func marshalCIReport(report CIReport) ([]byte, error) {
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode CI report: %w", err)
	}
	return append(payload, '\n'), nil
}

func writeCIReport(workspace *resources.Workspace, outputDirectory string, report CIReport) (string, []byte, error) {
	if workspace == nil {
		return "", nil, fmt.Errorf("write CI report: workspace is nil")
	}
	outputDirectory = strings.TrimSpace(outputDirectory)
	if outputDirectory == "" {
		return "", nil, fmt.Errorf("write CI report: output directory is empty")
	}
	outputDirectory = resolveCIOutputDirectory(workspace, outputDirectory)
	destination := filepath.Clean(filepath.Join(outputDirectory, reportFilename))
	payload, err := marshalCIReport(report)
	if err != nil {
		return destination, nil, err
	}
	if err := writeCIReportAtomic(destination, payload); err != nil {
		return destination, nil, fmt.Errorf("write CI report %s: %w", destination, err)
	}
	return destination, payload, nil
}

func resolveCIOutputDirectory(workspace *resources.Workspace, outputDirectory string) string {
	if !filepath.IsAbs(outputDirectory) {
		outputDirectory = filepath.Join(workspace.Dir(), outputDirectory)
	}
	return filepath.Clean(outputDirectory)
}

func writeCIArtifact(workspace *resources.Workspace, relative string, payload []byte) (string, error) {
	if workspace == nil {
		return "", fmt.Errorf("write CI artifact: workspace is nil")
	}
	relative = filepath.Clean(filepath.FromSlash(strings.TrimSpace(relative)))
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid CI artifact path %q", relative)
	}
	destination := filepath.Join(resolveCIOutputDirectory(workspace, ciReportOutput), relative)
	if err := writeCIReportAtomic(destination, payload); err != nil {
		return "", fmt.Errorf("write CI artifact %s: %w", destination, err)
	}
	return filepath.ToSlash(relative), nil
}

func writeCIReportAtomic(destination string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".codefly-ci-report-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

// runWithCIReport owns output suppression, finalization, atomic artifact
// writing, and rendering for every executable CI command.
func runWithCIReport(ctx context.Context, workspace *resources.Workspace, plan *Plan, command string, operation func(*CIReporter) error) (result error) {
	format, err := normalizeCIReportFormat(ciReportFormat)
	if err != nil {
		return err
	}
	reporter, err := NewCIReporter(plan, command)
	if err != nil {
		return err
	}

	if format == "json" {
		cli.SuppressOutput()
		cli.SetOutputSink(func(wool.Loglevel, string) {})
		defer func() {
			cli.SetOutputSink(nil)
			cli.RestoreOutput()
		}()
	}

	if operation != nil {
		result = operation(reporter)
	}
	if result == nil {
		result = ctx.Err()
	}
	report := reporter.Finalize(result)
	destination, payload, reportErr := writeCIReport(workspace, ciReportOutput, report)
	result = errors.Join(result, reportErr)

	if format == "json" {
		if len(payload) > 0 {
			_, _ = os.Stdout.Write(payload)
		}
		if result != nil && len(payload) > 0 {
			return machineReadableCIError{error: result}
		}
		return result
	}
	if reportErr == nil {
		cli.Header(1, "Codefly CI %s: %d passed, %d failed, %d skipped, %d cancelled", report.Status,
			report.Summary.Passed, report.Summary.Failed, report.Summary.Skipped, report.Summary.Cancelled)
		cli.Info("Report: %s", destination)
	}
	return result
}

func normalizeCIReportFormat(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "text":
		return "text", nil
	case "json":
		return "json", nil
	default:
		return "", fmt.Errorf("unsupported CI report format %q (use text or json)", value)
	}
}
