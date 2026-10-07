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
	reportSchemaVersion = 3
	reportFilename      = "report.json"

	reportStatusPending   = "pending"
	reportStatusRunning   = "running"
	reportStatusPassed    = "passed"
	reportStatusFailed    = "failed"
	reportStatusSkipped   = "skipped"
	reportStatusCancelled = "cancelled"
	reportStatusReused    = "reused"

	reportReasonFailedPrerequisite       = "failed_prerequisite"
	reportReasonFailFast                 = "fail_fast"
	reportReasonRunCancelled             = "run_cancelled"
	reportReasonNotScheduled             = "not_scheduled"
	reportReasonAgentNoSyncCapability    = "agent_no_sync_capability"
	reportReasonAgentNoTestCapability    = "agent_no_test_capability"
	reportReasonAgentNoLintCapability    = "agent_no_lint_capability"
	reportReasonAgentNoCompileCapability = "agent_no_compile_capability"

	// artifactSubjectSource is evidence about the checked-out source and its
	// declared dependencies; artifactSubjectImage is evidence about a runtime
	// image, bound to the digest and platform that were actually scanned.
	// artifactSubjectUnknown is the explicit subject for evidence whose producer
	// did not name one: it is recorded rather than left absent so a consumer
	// never has to infer meaning from a missing key, and so an unnamed subject
	// can never be mistaken for runtime-image coverage.
	artifactSubjectSource  = "source"
	artifactSubjectImage   = "image"
	artifactSubjectUnknown = "unknown"
)

// Report is Codefly's provider-neutral record of one CI command. Task order
// is invocation order, then affected-service plan order; it never depends on
// concurrent completion order.
type Report struct {
	SchemaVersion  int           `json:"schema_version"`
	Command        string        `json:"command"`
	CodeflyVersion string        `json:"codefly_version"`
	Plan           Plan          `json:"plan"`
	Phases         []string      `json:"phases"`
	Status         string        `json:"status"`
	StartedAt      string        `json:"started_at"`
	FinishedAt     string        `json:"finished_at"`
	DurationMS     int64         `json:"duration_ms"`
	Summary        ReportSummary `json:"summary"`
	Tasks          []ReportTask  `json:"tasks"`
	Error          string        `json:"error,omitempty"`
}

// ReportSummary keeps executed success and verified reuse apart: Passed
// counts tasks this run actually executed, Reused counts tasks that stood on a
// verified earlier execution, and Skipped never means either.
type ReportSummary struct {
	Total     int `json:"total"`
	Passed    int `json:"passed"`
	Reused    int `json:"reused"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Cancelled int `json:"cancelled"`
}

// ReportTask describes the logical operation even when it never executes.
// ID is stable for a given phase, optional suite, and service; a future cache
// content digest can therefore attach to the task without changing identity.
type ReportTask struct {
	ID               string           `json:"id"`
	Scope            string           `json:"scope"`
	Resource         string           `json:"resource"`
	Phase            string           `json:"phase"`
	Stage            resources.Stage  `json:"stage,omitempty"`
	Suite            string           `json:"suite,omitempty"`
	Service          string           `json:"service,omitempty"`
	Classification   string           `json:"classification,omitempty"`
	SelectionReasons []string         `json:"selection_reasons"`
	Prerequisites    []string         `json:"prerequisites"`
	RuntimeResources []string         `json:"runtime_resources"`
	Cache            CacheIdentity    `json:"cache"`
	Audit            *ReportAudit     `json:"audit,omitempty"`
	Drift            *ReportDrift     `json:"drift,omitempty"`
	Artifacts        []ReportArtifact `json:"artifacts,omitempty"`
	Status           string           `json:"status"`
	StatusReason     string           `json:"status_reason,omitempty"`
	BlockedBy        []string         `json:"blocked_by,omitempty"`
	StartedAt        string           `json:"started_at,omitempty"`
	FinishedAt       string           `json:"finished_at,omitempty"`
	DurationMS       int64            `json:"duration_ms"`
	Error            string           `json:"error,omitempty"`
}

type ReportAudit struct {
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

type ReportDrift struct {
	ChangedFiles []string `json:"changed_files"`
}

// ReportArtifact names one piece of evidence a task produced. Subject states
// what the evidence describes: a source inventory and a runtime-image inventory
// are different claims, and a consumer that cannot tell them apart reads a
// lockfile scan as proof the shipped image was scanned. Subject is always
// written, never omitted — evidence whose producer named no subject is recorded
// as artifactSubjectUnknown, so "unknown" is a value a consumer can read rather
// than a missing key it has to interpret. It is deliberately not called "scope":
// ReportTask.Scope is resource ownership, and one report must not use the
// same key for two vocabularies.
//
// Image evidence additionally carries the digest and platform it was scanned
// from. One scan can satisfy several services at once, so an identical digest is
// scanned and stored once and Associations is what keeps every service's claim
// on it rather than collapsing them.
type ReportArtifact struct {
	Kind         string             `json:"kind"`
	Subject      string             `json:"subject"`
	Path         string             `json:"path"`
	MediaType    string             `json:"media_type,omitempty"`
	SHA256       string             `json:"sha256"`
	Digest       string             `json:"digest,omitempty"`
	Platform     string             `json:"platform,omitempty"`
	Associations []ImageAssociation `json:"associations,omitempty"`
}

// ImageAssociation names one service-owned image that a single scan
// covers, in the role the image plays for that service.
type ImageAssociation struct {
	Service   string `json:"service"`
	Role      string `json:"role,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// cloneCIReportArtifacts deep-copies recorded evidence. Artifacts were all
// scalar until image evidence added a slice, so a plain copy would leave a
// finalized report sharing associations with live reporter state.
func cloneCIReportArtifacts(artifacts []ReportArtifact) []ReportArtifact {
	if artifacts == nil {
		return nil
	}
	cloned := append([]ReportArtifact(nil), artifacts...)
	for index := range artifacts {
		cloned[index].Associations = append([]ImageAssociation(nil), artifacts[index].Associations...)
	}
	return cloned
}

// normalizeCIReportSubject guarantees the subject invariant at every boundary
// where evidence enters a report, so the report can state the invariant without
// depending on each producer to remember it.
func normalizeCIReportSubject(artifact *ReportArtifact) {
	if strings.TrimSpace(artifact.Subject) == "" {
		artifact.Subject = artifactSubjectUnknown
	}
}

// normalizedCIReportArtifacts copies and normalizes recorded evidence. Reused
// tasks reach the report through this path instead of recordCIReportArtifact,
// and they must satisfy the same invariant.
func normalizedCIReportArtifacts(artifacts []ReportArtifact) []ReportArtifact {
	if artifacts == nil {
		return nil
	}
	normalized := cloneCIReportArtifacts(artifacts)
	for index := range normalized {
		normalizeCIReportSubject(&normalized[index])
	}
	return normalized
}

type ciReportTaskContextKey struct{}

type machineReadableCIError struct {
	error
}

func (machineReadableCIError) MachineReadable() bool { return true }

type ciReportTaskContext struct {
	reporter *Reporter
	id       string
}

func withCIReportTask(ctx context.Context, reporter *Reporter, id string) context.Context {
	if reporter == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, ciReportTaskContextKey{}, ciReportTaskContext{reporter: reporter, id: id})
}

func recordCIReportAudit(ctx context.Context, audit *ReportAudit) {
	task, ok := ctx.Value(ciReportTaskContextKey{}).(ciReportTaskContext)
	if !ok || task.reporter == nil {
		return
	}
	task.reporter.mu.Lock()
	defer task.reporter.mu.Unlock()
	if reportTask, found := task.reporter.task(task.id); found {
		audited := *audit
		reportTask.Audit = &audited
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
		reportTask.Drift = &ReportDrift{ChangedFiles: cloneStrings(changed)}
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

func recordCIReportArtifact(ctx context.Context, artifact *ReportArtifact) {
	task, ok := ctx.Value(ciReportTaskContextKey{}).(ciReportTaskContext)
	if !ok || task.reporter == nil {
		return
	}
	normalizeCIReportSubject(artifact)
	task.reporter.mu.Lock()
	defer task.reporter.mu.Unlock()
	if reportTask, found := task.reporter.task(task.id); found {
		reportTask.Artifacts = append(reportTask.Artifacts, *artifact)
	}
}

type reportClock func() time.Time

type Reporter struct {
	mu           sync.Mutex
	now          reportClock
	startedAt    time.Time
	report       Report
	taskIndex    map[string]int
	cacheBuilder *ciCacheIdentityBuilder
	reuse        *ciResultReuse
}

func NewCIReporter(plan *Plan, command string) (*Reporter, error) {
	version, err := cli.GetCurrentVersion()
	if err != nil {
		return nil, fmt.Errorf("read Codefly version for CI report: %w", err)
	}
	return newCIReporter(plan, command, version, time.Now)
}

func newCIReporter(plan *Plan, command, version string, now reportClock) (*Reporter, error) {
	if plan == nil {
		return nil, fmt.Errorf("CI report plan is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("CI report clock is nil")
	}
	startedAt := now().UTC()
	reporter := &Reporter{
		now:       now,
		startedAt: startedAt,
		taskIndex: map[string]int{},
		report: Report{
			SchemaVersion:  reportSchemaVersion,
			Command:        strings.TrimSpace(command),
			CodeflyVersion: strings.TrimSpace(version),
			Plan:           clonePlan(plan),
			Phases:         []string{},
			Status:         reportStatusRunning,
			StartedAt:      formatReportTime(startedAt),
			Tasks:          []ReportTask{},
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
		if phase == ciPhaseTest {
			suite = "default"
		} else {
			return phase + ":" + service
		}
	}
	return phase + ":" + suite + ":" + service
}

func (reporter *Reporter) registerTasks(ctx context.Context, workspace *resources.Workspace, options ScheduleOptions, tasks []ciScheduledTask) ([]string, error) {
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
		reporter.cacheBuilder = newCICacheIdentityBuilder(ctx, workspace, reporter.report.CodeflyVersion, reporter.reuseEnvironment())
	}

	ids := make([]string, len(tasks))
	for index := range tasks {
		task := &tasks[index]
		id := reportTaskID(phase, options.Suite, task.planned.Service)
		if _, exists := reporter.taskIndex[id]; exists {
			ids[index] = id
			continue
		}
		ids[index] = id
		reporter.taskIndex[id] = len(reporter.report.Tasks)
		cacheIdentity := reporter.cacheBuilder.identity(ctx, options, &task.planned)
		reporter.report.Tasks = append(reporter.report.Tasks, ReportTask{
			ID:               id,
			Scope:            "service",
			Resource:         task.planned.Service,
			Phase:            phase,
			Suite:            strings.TrimSpace(options.Suite),
			Service:          task.planned.Service,
			Classification:   task.planned.Classification,
			SelectionReasons: cloneStrings(task.planned.Reasons),
			Stage:            scheduleStage(options),
			Prerequisites:    cloneStrings(task.prerequisites),
			RuntimeResources: cloneStrings(task.resources),
			Cache:            cacheIdentity,
			Status:           reportStatusPending,
		})
	}
	return ids, nil
}

func (reporter *Reporter) registerWorkspaceTask(ctx context.Context, workspace *resources.Workspace, phase string) (string, error) {
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
		reporter.cacheBuilder = newCICacheIdentityBuilder(ctx, workspace, reporter.report.CodeflyVersion, reporter.reuseEnvironment())
	}
	options := ScheduleOptions{Phase: phase, RuntimeContext: runtimeContext}
	cacheIdentity := reporter.cacheBuilder.workspaceIdentity(options, workspace.Name)
	reporter.taskIndex[id] = len(reporter.report.Tasks)
	reporter.report.Tasks = append(reporter.report.Tasks, ReportTask{
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

func runReportedWorkspacePhase(ctx context.Context, reporter *Reporter, workspace *resources.Workspace, phase string, action func(context.Context) error) error {
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
	tasks, err := buildScheduledTasks(ctx, workspace, plan, options)
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

func (reporter *Reporter) startTask(id string) {
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

func (reporter *Reporter) finishTask(id string, err error) {
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
func (reporter *Reporter) markSkipped(id, reason string) {
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

func (reporter *Reporter) skipTask(id, reason string, blockedBy []string) {
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

func (reporter *Reporter) cancelPendingTask(id string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	task, ok := reporter.task(id)
	if !ok || task.Status != reportStatusPending {
		return
	}
	task.Status = reportStatusCancelled
	task.StatusReason = reportReasonRunCancelled
}

func (reporter *Reporter) task(id string) (*ReportTask, bool) {
	index, ok := reporter.taskIndex[id]
	if !ok || index < 0 || index >= len(reporter.report.Tasks) {
		return nil, false
	}
	return &reporter.report.Tasks[index], true
}

func (reporter *Reporter) Finalize(runErr error) Report {
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
	return cloneCIReport(&reporter.report)
}

func summarizeReportTasks(tasks []ReportTask) ReportSummary {
	summary := ReportSummary{Total: len(tasks)}
	for index := range tasks {
		task := &tasks[index]
		switch task.Status {
		case reportStatusPassed:
			summary.Passed++
		case reportStatusReused:
			summary.Reused++
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

func cloneCIReport(report *Report) Report {
	// A copy of the report, not a view of it: the reporter keeps writing the
	// original, and the clone replaces every slice the original holds.
	cloned := *report
	cloned.Plan = clonePlan(&report.Plan)
	cloned.Phases = append([]string(nil), report.Phases...)
	cloned.Tasks = make([]ReportTask, len(report.Tasks))
	for index := range report.Tasks {
		task := &report.Tasks[index]
		cloned.Tasks[index] = *task
		cloned.Tasks[index].SelectionReasons = cloneStrings(task.SelectionReasons)
		cloned.Tasks[index].Prerequisites = cloneStrings(task.Prerequisites)
		cloned.Tasks[index].RuntimeResources = cloneStrings(task.RuntimeResources)
		cloned.Tasks[index].BlockedBy = append([]string(nil), task.BlockedBy...)
		cloned.Tasks[index].Cache.Inputs.Dependencies = append([]CacheResourceDigest{}, task.Cache.Inputs.Dependencies...)
		cloned.Tasks[index].Cache.Inputs.Libraries = append([]CacheResourceDigest{}, task.Cache.Inputs.Libraries...)
		cloned.Tasks[index].Cache.Limitations = append([]string(nil), task.Cache.Limitations...)
		if task.Cache.Reuse != nil {
			reuse := *task.Cache.Reuse
			reuse.Artifacts = cloneCIReportArtifacts(task.Cache.Reuse.Artifacts)
			cloned.Tasks[index].Cache.Reuse = &reuse
		}
		if task.Audit != nil {
			audit := *task.Audit
			cloned.Tasks[index].Audit = &audit
		}
		if task.Drift != nil {
			cloned.Tasks[index].Drift = &ReportDrift{ChangedFiles: cloneStrings(task.Drift.ChangedFiles)}
		}
		cloned.Tasks[index].Artifacts = cloneCIReportArtifacts(task.Artifacts)
	}
	return cloned
}

func marshalCIReport(report *Report) ([]byte, error) {
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode CI report: %w", err)
	}
	return append(payload, '\n'), nil
}

func writeCIReport(workspace *resources.Workspace, outputDirectory string, report *Report) (string, []byte, error) {
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
func runWithCIReport(ctx context.Context, workspace *resources.Workspace, plan *Plan, command string, operation func(*Reporter) error) (result error) {
	format, err := normalizeCIReportFormat(ciReportFormat)
	if err != nil {
		return err
	}
	reporter, err := NewCIReporter(plan, command)
	if err != nil {
		return err
	}
	reporter.reuse, err = newCIResultReuse(ctx, workspace, &ciReuse)
	if err != nil {
		return err
	}

	if format == prereleaseFormatJSON {
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
	destination, payload, reportErr := writeCIReport(workspace, ciReportOutput, &report)
	result = errors.Join(result, reportErr)

	if format == prereleaseFormatJSON {
		if len(payload) > 0 {
			_, _ = os.Stdout.Write(payload)
		}
		if result != nil && len(payload) > 0 {
			return machineReadableCIError{error: result}
		}
		return result
	}
	if reportErr == nil {
		cli.Header(1, "Codefly CI %s: %d passed, %d reused, %d failed, %d skipped, %d cancelled", report.Status,
			report.Summary.Passed, report.Summary.Reused, report.Summary.Failed, report.Summary.Skipped, report.Summary.Cancelled)
		cli.Info("Report: %s", destination)
	}
	return result
}

func normalizeCIReportFormat(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", prereleaseFormatText:
		return prereleaseFormatText, nil
	case prereleaseFormatJSON:
		return prereleaseFormatJSON, nil
	default:
		return "", fmt.Errorf("unsupported CI report format %q (use text or json)", value)
	}
}

func (reporter *Reporter) reuseEnvironment() string {
	if reporter.reuse == nil {
		return ""
	}
	return reporter.reuse.environment
}

// attemptReuse decides whether the task bound to id may stand on a previously
// verified execution, and restores its artifacts when it may. Any rejection —
// missing, stale, untrusted, unreadable or unrestorable — leaves the task
// running so the caller executes it.
func (reporter *Reporter) attemptReuse(id string) bool {
	reporter.mu.Lock()
	reuse := reporter.reuse
	task, ok := reporter.task(id)
	if reuse == nil || !ok || task.Status != reportStatusRunning {
		reporter.mu.Unlock()
		return false
	}
	identity := task.Cache
	phase := task.Phase
	reporter.mu.Unlock()

	decision := reuse.lookup(&identity, phase)
	if decision.record == nil {
		reporter.noteCacheDecision(id, decision.status, decision.reason)
		return false
	}
	if err := reuse.restore(decision.record); err != nil {
		reporter.noteCacheDecision(id, cacheStatusMiss, err.Error())
		return false
	}
	record := decision.record

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	task, ok = reporter.task(id)
	if !ok || task.Status != reportStatusRunning {
		return false
	}
	now := reporter.now().UTC()
	task.Status = reportStatusReused
	task.FinishedAt = formatReportTime(now)
	task.DurationMS = reportDurationMS(task.StartedAt, now)
	task.Audit = record.Evidence.Audit
	task.Drift = record.Evidence.Drift
	task.Artifacts = normalizedCIReportArtifacts(record.Evidence.Artifacts)
	task.Cache.Status = cacheStatusHit
	task.Cache.Reuse = &CacheReuse{
		Reference:  record.Reference,
		Run:        record.Run,
		Revision:   record.Revision,
		Identity:   record.Identity,
		RecordedAt: record.RecordedAt,
		Artifacts:  normalizedCIReportArtifacts(record.Evidence.Artifacts),
	}
	return true
}

// publishResult records a task this run actually executed and passed. Publishing
// is an optimization for later runs: a storage failure is reported against the
// task's cache status and never turns a real success into a failure.
func (reporter *Reporter) publishResult(id string) {
	reporter.mu.Lock()
	reuse := reporter.reuse
	task, ok := reporter.task(id)
	if reuse == nil || !ok || task.Status != reportStatusPassed {
		reporter.mu.Unlock()
		return
	}
	if reason := reuse.publishBlockedReason(); reason != "" {
		reporter.mu.Unlock()
		reporter.noteCacheDecision(id, "", reason)
		return
	}
	if !reuseVerifiableOutputs(task.Phase) {
		reporter.mu.Unlock()
		return
	}
	if eligible, reason := task.Cache.reuseEligibility(); !eligible {
		reporter.mu.Unlock()
		reporter.noteCacheDecision(id, cacheStatusIneligible, reason)
		return
	}
	snapshot := *task
	snapshot.Artifacts = cloneCIReportArtifacts(task.Artifacts)
	reporter.mu.Unlock()

	if err := reuse.publish(&snapshot); err != nil {
		reporter.noteCacheDecision(id, "", "result was not published: "+err.Error())
		return
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if task, found := reporter.task(id); found {
		task.Cache.Stored = true
	}
}

// noteCacheDecision records what this run decided about the task's cache entry.
// An empty status or reason leaves the recorded one in place, so publishing a
// freshly executed result does not erase why its predecessor was not reusable.
func (reporter *Reporter) noteCacheDecision(id, status, reason string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	task, ok := reporter.task(id)
	if !ok {
		return
	}
	if status != "" {
		task.Cache.Status = status
	}
	if reason != "" {
		task.Cache.StatusReason = reason
	}
}
