package ci

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

const reuseTestReference = "refs/heads/main"

func TestVerifiedReuseExecutesColdThenReusesWarmAndInvalidatesOnInputChange(t *testing.T) {
	root, workspace := loadReuseFixture(t)
	store := t.TempDir()
	plan := cacheTestPlan(workspace, "management/consumer")

	cold := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference))
	if cold.executed != 1 {
		t.Fatalf("cold run executed %d tasks, want 1", cold.executed)
	}
	if task := cold.task(t); task.Cache.Status != cacheStatusMiss || !task.Cache.Stored {
		t.Fatalf("cold cache = %q stored=%v (%s)", task.Cache.Status, task.Cache.Stored, task.Cache.StatusReason)
	}
	if cold.report.Summary.Passed != 1 || cold.report.Summary.Reused != 0 {
		t.Fatalf("cold summary = %#v", cold.report.Summary)
	}

	warm := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference))
	if warm.executed != 0 {
		t.Fatalf("warm run executed %d tasks, want 0", warm.executed)
	}
	task := warm.task(t)
	if task.Status != reportStatusReused || task.Cache.Status != cacheStatusHit {
		t.Fatalf("warm task = %s/%s (%s)", task.Status, task.Cache.Status, task.Cache.StatusReason)
	}
	if warm.report.Summary.Reused != 1 || warm.report.Summary.Passed != 0 {
		t.Fatalf("warm summary = %#v", warm.report.Summary)
	}
	if task.Cache.Reuse == nil {
		t.Fatal("reused task carries no reuse evidence")
	}
	if task.Cache.Reuse.Reference != reuseTestReference || task.Cache.Reuse.Run != "run-1" {
		t.Fatalf("reuse evidence = %#v", task.Cache.Reuse)
	}
	if task.Cache.Reuse.Identity != task.Cache.Key || task.Cache.Reuse.RecordedAt == "" {
		t.Fatalf("reuse evidence does not bind the matched identity: %#v", task.Cache.Reuse)
	}

	environment := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:bbb", reuseTestReference))
	if environment.executed != 1 {
		t.Fatal("a different execution environment reused a result")
	}

	for name, mutate := range map[string]func(){
		"target source": func() {
			writeCacheTestFile(t, filepath.Join(root, "modules", "management", "services", "consumer", "code", "consumer.txt"), "changed")
		},
		"unattributed repository file": func() {
			writeCacheTestFile(t, filepath.Join(root, "tools", "shared-fixture.json"), `{"seed":2}`)
		},
	} {
		mutate()
		runCacheTestGit(t, root, "add", "-A")
		changed := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference))
		if changed.executed != 1 {
			t.Fatalf("%s did not invalidate the reused result", name)
		}
	}
}

func TestVerifiedReuseMatchesDefaultAndNamedTestSuites(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	plan := cacheTestPlan(workspace, "management/consumer")
	store := t.TempDir()
	for _, suite := range []string{"", "unit", "integration"} {
		t.Run(firstNonEmpty(suite, "default"), func(t *testing.T) {
			options := []reuseGateOption{withReusePhase("test"), withReuseSuite(suite)}
			cold := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), options...)
			if cold.executed != 1 || !cold.task(t).Cache.Stored {
				t.Fatalf("new suite did not execute and publish: %#v", cold.task(t).Cache)
			}
			warm := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), options...)
			if warm.executed != 0 || warm.task(t).Status != reportStatusReused {
				t.Fatalf("matching suite did not reuse: %#v", warm.task(t).Cache)
			}
			if warm.task(t).ID != reportTaskID("test", suite, "management/consumer") {
				t.Fatalf("reused task ID = %q", warm.task(t).ID)
			}
		})
	}
}

func TestVerifiedReuseRestoresAndVerifiesRequiredArtifacts(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	store := t.TempDir()
	plan := cacheTestPlan(workspace, "management/consumer")
	artifact := filepath.Join("sbom", "management--consumer.cdx.json")
	payload := []byte("{\"bomFormat\":\"CycloneDX\"}\n")

	produce := func(ctx context.Context, _ *resources.Workspace, _ *resources.Module, _ *resources.Service) error {
		relative, err := writeCIArtifact(workspace, artifact, payload)
		if err != nil {
			return err
		}
		recordCIReportArtifact(ctx, CIReportArtifact{Kind: "cyclonedx-sbom", Path: relative, MediaType: "application/vnd.cyclonedx+json", SHA256: artifactDigest(payload)})
		return nil
	}
	cold := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), withReuseAction(produce), withReusePhase("sbom"))
	if cold.executed != 1 || !cold.task(t).Cache.Stored {
		t.Fatalf("cold sbom task = %#v", cold.task(t).Cache)
	}

	restored := filepath.Join(workspace.Dir(), ".codefly", "ci", artifact)
	if err := os.Remove(restored); err != nil {
		t.Fatal(err)
	}
	warm := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), withReusePhase("sbom"))
	if warm.executed != 0 {
		t.Fatal("warm sbom run executed instead of reusing")
	}
	content, err := os.ReadFile(restored)
	if err != nil {
		t.Fatalf("required artifact was not restored: %v", err)
	}
	if string(content) != string(payload) {
		t.Fatalf("restored artifact = %q", content)
	}
	if len(warm.task(t).Artifacts) != 1 || warm.task(t).Artifacts[0].SHA256 != artifactDigest(payload) {
		t.Fatalf("reused task artifacts = %#v", warm.task(t).Artifacts)
	}
}

func TestVerifiedReuseFallsBackToExecutionWhenEvidenceIsUnusable(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	plan := cacheTestPlan(workspace, "management/consumer")

	for name, damage := range map[string]func(t *testing.T, store string, record *ciResultRecord){
		"forged signature": func(t *testing.T, store string, record *ciResultRecord) {
			record.Signature = "hmac-sha256:" + strings.Repeat("00", 32)
			writeReuseRecord(t, store, record)
		},
		"untrusted producing reference": func(t *testing.T, store string, record *ciResultRecord) {
			record.Reference = "refs/pull/7/merge"
			resignReuseRecord(t, store, record)
		},
		"missing run": func(t *testing.T, store string, record *ciResultRecord) {
			record.Run = ""
			resignReuseRecord(t, store, record)
		},
		"missing task": func(t *testing.T, store string, record *ciResultRecord) {
			record.Task = ""
			resignReuseRecord(t, store, record)
		},
		"different task": func(t *testing.T, store string, record *ciResultRecord) {
			record.Task = "compile:management/worker"
			resignReuseRecord(t, store, record)
		},
		"different phase": func(t *testing.T, store string, record *ciResultRecord) {
			record.Phase = "lint"
			resignReuseRecord(t, store, record)
		},
		"different suite": func(t *testing.T, store string, record *ciResultRecord) {
			record.Suite = "integration"
			resignReuseRecord(t, store, record)
		},
		"different service": func(t *testing.T, store string, record *ciResultRecord) {
			record.Service = "management/worker"
			resignReuseRecord(t, store, record)
		},
		"future success": func(t *testing.T, store string, record *ciResultRecord) {
			record.RecordedAt = formatReportTime(time.Now().Add(time.Hour))
			resignReuseRecord(t, store, record)
		},
		"recorded failure": func(t *testing.T, store string, record *ciResultRecord) {
			record.Outcome = "failed"
			resignReuseRecord(t, store, record)
		},
		"incompatible identity contract": func(t *testing.T, store string, record *ciResultRecord) {
			record.IdentitySchema = cacheIdentitySchemaVersion + 1
			resignReuseRecord(t, store, record)
		},
		"malformed record": func(t *testing.T, store string, record *ciResultRecord) {
			writeCacheTestFile(t, (&ciResultStore{root: store}).recordPath(record.Identity), "{not json")
		},
		"vanished artifact": func(t *testing.T, store string, record *ciResultRecord) {
			path, err := (&ciResultStore{root: store}).blobPath(record.Evidence.Artifacts[0].SHA256)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
		"tampered artifact": func(t *testing.T, store string, record *ciResultRecord) {
			path, err := (&ciResultStore{root: store}).blobPath(record.Evidence.Artifacts[0].SHA256)
			if err != nil {
				t.Fatal(err)
			}
			writeCacheTestFile(t, path, "replaced payload")
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := t.TempDir()
			payload := []byte("evidence\n")
			produce := func(ctx context.Context, _ *resources.Workspace, _ *resources.Module, _ *resources.Service) error {
				relative, err := writeCIArtifact(workspace, filepath.Join("sbom", "evidence.json"), payload)
				if err != nil {
					return err
				}
				recordCIReportArtifact(ctx, CIReportArtifact{Kind: "cyclonedx-sbom", Path: relative, SHA256: artifactDigest(payload)})
				return nil
			}
			cold := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), withReuseAction(produce))
			if !cold.task(t).Cache.Stored {
				t.Fatalf("cold run did not publish: %#v", cold.task(t).Cache)
			}
			damage(t, store, readReuseRecord(t, store, cold.task(t).Cache.Key))

			warm := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), withReuseAction(produce))
			if warm.executed != 1 {
				t.Fatalf("%s was accepted as a verified result", name)
			}
			if warm.task(t).Status != reportStatusPassed {
				t.Fatalf("task status = %s", warm.task(t).Status)
			}
			if warm.task(t).Cache.Status != cacheStatusMiss || warm.task(t).Cache.StatusReason == "" {
				t.Fatalf("%s was not reported as an unusable record: %#v", name, warm.task(t).Cache)
			}
		})
	}
}

func TestVerifiedReuseNeverPublishesFromUntrustedReference(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	store := t.TempDir()
	plan := cacheTestPlan(workspace, "management/consumer")

	pull := newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", "refs/pull/7/merge")
	run := runReuseGate(t, workspace, plan, pull)
	if run.executed != 1 {
		t.Fatal("pull-request run did not execute")
	}
	if run.task(t).Cache.Stored {
		t.Fatal("pull-request run published a result")
	}
	if record, err := (&ciResultStore{root: store}).lookup(run.task(t).Cache.Key); err != nil || record != nil {
		t.Fatalf("untrusted run seeded the store: %#v %v", record, err)
	}
}

func TestVerifiedReuseAlwaysReExecutesTimeSensitiveAudits(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	store := t.TempDir()
	plan := cacheTestPlan(workspace, "management/consumer")

	cold := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), withReusePhase("audit"))
	if cold.executed != 1 {
		t.Fatal("cold audit did not execute")
	}
	warm := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), withReusePhase("audit"))
	if warm.executed != 1 {
		t.Fatal("audit reused a result without advisory freshness")
	}
	if warm.task(t).Cache.Status != cacheStatusIneligible || !strings.Contains(warm.task(t).Cache.StatusReason, "changes independently") {
		t.Fatalf("audit rejection = %#v", warm.task(t).Cache)
	}
}

func TestVerifiedReuseExpiresResultsOlderThanTheConfiguredWindow(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	store := t.TempDir()
	plan := cacheTestPlan(workspace, "management/consumer")

	cold := newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference)
	cold.maxAge = time.Hour
	cold.now = func() time.Time { return time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC) }
	if runReuseGate(t, workspace, plan, cold).executed != 1 {
		t.Fatal("cold run did not execute")
	}

	expired := newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference)
	expired.maxAge = time.Hour
	expired.now = func() time.Time { return time.Date(2026, time.July, 16, 14, 0, 0, 0, time.UTC) }
	warm := runReuseGate(t, workspace, plan, expired)
	if warm.executed != 1 {
		t.Fatal("an expired result was reused")
	}
	if !strings.Contains(warm.task(t).Cache.StatusReason, "older than") {
		t.Fatalf("expiry reason = %q", warm.task(t).Cache.StatusReason)
	}
}

func TestVerifiedReuseRequiresAnExplicitTrustAndEnvironmentScope(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	t.Setenv(ciResultKeyVariable, "reuse-test-signing-key")
	store := t.TempDir()
	complete := ciReuseFlags{
		run:               "run-1",
		enabled:           true,
		store:             store,
		environment:       "runner@sha256:aaa",
		reference:         reuseTestReference,
		trustedReferences: []string{reuseTestReference},
		maxAge:            time.Hour,
	}
	for name, mutate := range map[string]func(flags *ciReuseFlags){
		"store":       func(flags *ciReuseFlags) { flags.store = "" },
		"environment": func(flags *ciReuseFlags) { flags.environment = "" },
		"trust scope": func(flags *ciReuseFlags) { flags.trustedReferences = nil },
	} {
		flags := complete
		mutate(&flags)
		if _, err := newCIResultReuse(context.Background(), workspace, &flags); err == nil {
			t.Fatalf("reuse accepted an unbounded %s", name)
		}
	}
	t.Setenv(ciResultKeyVariable, "")
	if _, err := newCIResultReuse(context.Background(), workspace, &complete); err == nil {
		t.Fatal("reuse accepted unauthenticated records")
	}
}

func TestVerifiedReusePublisherRequiresRunProvenance(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	t.Setenv(ciResultKeyVariable, "reuse-test-signing-key")
	t.Setenv("CODEFLY_CI_RUN", "")
	flags := ciReuseFlags{
		enabled: true, store: t.TempDir(), environment: "runner@sha256:aaa",
		reference: reuseTestReference, trustedReferences: []string{reuseTestReference},
		maxAge: time.Hour,
	}
	if _, err := newCIResultReuse(context.Background(), workspace, &flags); err == nil || !strings.Contains(err.Error(), "--reuse-run") {
		t.Fatalf("missing publisher run error = %v", err)
	}
	flags.reference = "refs/pull/7/merge"
	if _, err := newCIResultReuse(context.Background(), workspace, &flags); err != nil {
		t.Fatalf("read-only reuse needs no publishing run: %v", err)
	}
	flags.reference = reuseTestReference
	t.Setenv("CODEFLY_CI_RUN", "hosted-run-123/attempt-2")
	reuse, err := newCIResultReuse(context.Background(), workspace, &flags)
	if err != nil || reuse.run != "hosted-run-123/attempt-2" {
		t.Fatalf("publisher run from environment = %v, %v", reuse, err)
	}
}

func TestCIResultStorePublishesCompleteRecordsUnderConcurrency(t *testing.T) {
	store := &ciResultStore{root: t.TempDir()}
	payloads := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
	var wait sync.WaitGroup
	for index, payload := range payloads {
		wait.Add(1)
		go func() {
			defer wait.Done()
			record := &ciResultRecord{
				Schema:         ciResultRecordSchema,
				IdentitySchema: cacheIdentitySchemaVersion,
				Identity:       "sha256:shared",
				Outcome:        ciResultOutcomePass,
				RecordedAt:     formatReportTime(time.Now()),
				Run:            string(rune('a' + index)),
				Evidence:       ciResultEvidence{Artifacts: []CIReportArtifact{{Path: "a.json", SHA256: artifactDigest(payload)}}},
			}
			if err := store.publish(record, map[string][]byte{artifactDigest(payload): payload}); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()

	record, err := store.lookup("sha256:shared")
	if err != nil || record == nil {
		t.Fatalf("lookup = %#v %v", record, err)
	}
	if record.Outcome != ciResultOutcomePass || len(record.Evidence.Artifacts) != 1 {
		t.Fatalf("published record is partial: %#v", record)
	}
	if _, err := store.blob(record.Evidence.Artifacts[0].SHA256); err != nil {
		t.Fatalf("published record names an unreadable artifact: %v", err)
	}
}

func TestCIResultStoreRefusesArtifactsThatDoNotMatchTheirDigest(t *testing.T) {
	store := &ciResultStore{root: t.TempDir()}
	record := &ciResultRecord{Schema: ciResultRecordSchema, Identity: "sha256:mismatch", Outcome: ciResultOutcomePass}
	err := store.publish(record, map[string][]byte{artifactDigest([]byte("declared")): []byte("actual")})
	if err == nil {
		t.Fatal("store published an artifact that does not match its digest")
	}
	if found, lookupErr := store.lookup("sha256:mismatch"); lookupErr != nil || found != nil {
		t.Fatal("a refused publication left a record behind")
	}
}

type reuseGateResult struct {
	report   CIReport
	executed int
}

func (result reuseGateResult) task(t *testing.T) CIReportTask {
	t.Helper()
	if len(result.report.Tasks) != 1 {
		t.Fatalf("report task count = %d, want 1", len(result.report.Tasks))
	}
	return result.report.Tasks[0]
}

type reuseGateOption func(*reuseGateSettings)

type reuseGateSettings struct {
	suite  string
	phase  string
	action Action
}

func withReusePhase(phase string) reuseGateOption {
	return func(settings *reuseGateSettings) { settings.phase = phase }
}

func withReuseSuite(suite string) reuseGateOption {
	return func(settings *reuseGateSettings) { settings.suite = suite }
}

func withReuseAction(action Action) reuseGateOption {
	return func(settings *reuseGateSettings) { settings.action = action }
}

func runReuseGate(t *testing.T, workspace *resources.Workspace, plan *Plan, reuse *ciResultReuse, options ...reuseGateOption) reuseGateResult {
	t.Helper()
	settings := reuseGateSettings{phase: "compile"}
	for _, option := range options {
		option(&settings)
	}
	executed := 0
	action := settings.action
	wrapped := func(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
		executed++
		if action == nil {
			return nil
		}
		return action(ctx, workspace, module, service)
	}
	reporter, err := newCIReporter(plan, "codefly ci run", "1.2.3", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	reporter.reuse = reuse
	scheduleOptions := ScheduleOptions{Jobs: 1, FailFast: true, Phase: settings.phase, Suite: settings.suite, RuntimeContext: "native", Reporter: reporter}
	if err := CIWithPlanOptions(context.Background(), workspace, plan, wrapped, scheduleOptions); err != nil {
		t.Fatal(err)
	}
	return reuseGateResult{report: reporter.Finalize(nil), executed: executed}
}

func newReuseTestEngine(t *testing.T, workspace *resources.Workspace, store, environment, reference string) *ciResultReuse {
	t.Helper()
	t.Setenv(ciResultKeyVariable, "reuse-test-signing-key")
	reuse, err := newCIResultReuse(context.Background(), workspace, &ciReuseFlags{
		enabled:           true,
		store:             store,
		environment:       environment,
		reference:         reference,
		trustedReferences: []string{reuseTestReference},
		run:               "run-1",
		maxAge:            time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return reuse
}

func loadReuseFixture(t *testing.T) (string, *resources.Workspace) {
	t.Helper()
	root, workspace := loadSchedulerFixture(t)
	t.Setenv("CODEFLY_HOME", t.TempDir())
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || entry.Name() != resources.ServiceConfigurationName {
			return walkErr
		}
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(path, []byte(strings.ReplaceAll(string(payload), "kind: runtime::service", "kind: codefly:service")), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", "-A")
	installReuseFixtureAgents(t, workspace)
	return root, workspace
}

func installReuseFixtureAgents(t *testing.T, workspace *resources.Workspace) {
	t.Helper()
	ctx := context.Background()
	services, _, err := loadPlanInventory(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range services {
		if record.service.Agent == nil {
			continue
		}
		path, err := record.service.Agent.Path(ctx)
		if err != nil {
			t.Fatal(err)
		}
		writeCacheTestFile(t, path, "fixture agent "+record.unique)
	}
}

func readReuseRecord(t *testing.T, store, identity string) *ciResultRecord {
	t.Helper()
	record, err := (&ciResultStore{root: store}).lookup(identity)
	if err != nil || record == nil {
		t.Fatalf("published record is missing: %#v %v", record, err)
	}
	return record
}

func resignReuseRecord(t *testing.T, store string, record *ciResultRecord) {
	t.Helper()
	signature, err := record.sign([]byte("reuse-test-signing-key"))
	if err != nil {
		t.Fatal(err)
	}
	record.Signature = signature
	writeReuseRecord(t, store, record)
}

func writeReuseRecord(t *testing.T, store string, record *ciResultRecord) {
	t.Helper()
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, (&ciResultStore{root: store}).recordPath(record.Identity), string(payload))
}

func TestVerifiedReuseIsScopedToTheAffectedServiceGate(t *testing.T) {
	reuseFlagNames := []string{"reuse-results", "reuse-store", "reuse-environment", "reuse-reference", "reuse-trusted-reference", "reuse-run", "reuse-max-age", "reuse-audit-max-age"}
	for _, name := range reuseFlagNames {
		if RunCmd.Flags().Lookup(name) == nil {
			t.Fatalf("ci run is missing %s", name)
		}
	}
	for _, command := range []*cobra.Command{BuildCmd, TestCmd, LintCmd, CompileCmd, PushCmd, DeployCmd} {
		for _, name := range reuseFlagNames {
			if command.Flags().Lookup(name) != nil {
				t.Fatalf("ci %s exposes %s: release and single-phase gates must keep full execution", command.Name(), name)
			}
		}
	}
}

func TestVerifiedReuseNeverReplacesWorkspaceVerification(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	store := t.TempDir()
	plan := cacheTestPlan(workspace, "management/consumer")

	executions := 0
	for range 2 {
		reporter, err := newCIReporter(plan, "codefly ci run", "1.2.3", time.Now)
		if err != nil {
			t.Fatal(err)
		}
		reporter.reuse = newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference)
		err = runReportedWorkspacePhase(context.Background(), reporter, workspace, ciPhaseVerify, func(context.Context) error {
			executions++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		report := reporter.Finalize(nil)
		if report.Tasks[0].Status != reportStatusPassed {
			t.Fatalf("workspace verification status = %s", report.Tasks[0].Status)
		}
	}
	if executions != 2 {
		t.Fatalf("workspace verification ran %d times, want 2", executions)
	}
}

func TestReuseEligibilityRejectsAnyUnboundInput(t *testing.T) {
	complete := CICacheIdentity{
		SchemaVersion: cacheIdentitySchemaVersion,
		Key:           "sha256:complete",
		Status:        cacheStatusIdentityOnly,
		Inputs: CICacheIdentityInput{
			Environment:    "runner@sha256:aaa",
			CLIDigest:      "sha256:cli",
			RepositoryRest: "sha256:rest",
			Agent:          CICacheAgentInput{Digest: "sha256:agent"},
		},
	}
	if eligible, reason := complete.reuseEligibility(); !eligible {
		t.Fatalf("complete identity rejected: %s", reason)
	}
	for name, mutate := range map[string]func(identity *CICacheIdentity){
		"older identity contract": func(identity *CICacheIdentity) {
			identity.SchemaVersion = cacheIdentitySchemaVersion - 1
		},
		"unavailable key": func(identity *CICacheIdentity) { identity.Key = "" },
		"unresolved input": func(identity *CICacheIdentity) {
			identity.Limitations = []string{"resolved agent binary is not installed"}
		},
		"unnamed environment": func(identity *CICacheIdentity) { identity.Inputs.Environment = "" },
		"unbound repository remainder": func(identity *CICacheIdentity) {
			identity.Inputs.RepositoryRest = ""
		},
		"unbound CLI binary":   func(identity *CICacheIdentity) { identity.Inputs.CLIDigest = "" },
		"unbound agent binary": func(identity *CICacheIdentity) { identity.Inputs.Agent.Digest = "" },
	} {
		identity := complete
		mutate(&identity)
		eligible, reason := identity.reuseEligibility()
		if eligible {
			t.Fatalf("%s was accepted as a reusable identity", name)
		}
		if reason == "" {
			t.Fatalf("%s was rejected without a reason", name)
		}
	}
}

func TestVerifiedReuseNeverStandsOnUnrecordedBuildOutputs(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	store := t.TempDir()
	plan := cacheTestPlan(workspace, "management/consumer")

	build := string(resources.PhaseBuild)
	cold := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), withReusePhase(build))
	for _, task := range cold.report.Tasks {
		if task.Cache.Stored {
			t.Fatalf("build task %s published an unrestorable result", task.ID)
		}
	}
	warm := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), withReusePhase(build))
	if warm.executed != cold.executed {
		t.Fatal("build reused a result whose container images Codefly never recorded")
	}
	for _, task := range warm.report.Tasks {
		if task.Cache.Status != cacheStatusIneligible {
			t.Fatalf("build task %s reuse status = %#v", task.ID, task.Cache)
		}
	}
}
