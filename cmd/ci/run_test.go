package ci

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunCmdRegistersPortIsolationFlags(t *testing.T) {
	for _, name := range []string{"temporary-ports", "override-port", "disposable"} {
		if RunCmd.Flags().Lookup(name) == nil {
			t.Fatalf("ci run must register --%s so agent conformance can isolate its port space", name)
		}
	}
}

func TestRunCIPhasesContinuesAfterFailureWhenFailFastDisabled(t *testing.T) {
	phases := []string{"sync-drift", "lint", "compile", "test"}
	var executed []string
	err := runCIPhases(context.Background(), phases, false, func(_ context.Context, phase string) error {
		executed = append(executed, phase)
		if phase == "lint" {
			return errors.New("lint failed")
		}
		return nil
	})
	if !reflect.DeepEqual(executed, phases) {
		t.Fatalf("executed phases = %v, want every phase to run despite early failure", executed)
	}
	if err == nil || !strings.Contains(err.Error(), "CI phase lint failed") {
		t.Fatalf("error = %v, want the lint failure to be reported", err)
	}
}

func TestRunCIPhasesAggregatesEveryPhaseFailureWhenFailFastDisabled(t *testing.T) {
	err := runCIPhases(context.Background(), []string{"sync-drift", "lint", "compile"}, false, func(_ context.Context, phase string) error {
		if phase == "lint" || phase == "compile" {
			return errors.New(phase + " broke")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected accumulated failures")
	}
	if !strings.Contains(err.Error(), "lint broke") || !strings.Contains(err.Error(), "compile broke") {
		t.Fatalf("error = %v, want both independent failures reported", err)
	}
}

func TestRunCIPhasesStopsAtFirstFailureWhenFailFastEnabled(t *testing.T) {
	var executed []string
	err := runCIPhases(context.Background(), []string{"lint", "compile"}, true, func(_ context.Context, phase string) error {
		executed = append(executed, phase)
		if phase == "lint" {
			return errors.New("lint failed")
		}
		return nil
	})
	if !reflect.DeepEqual(executed, []string{"lint"}) {
		t.Fatalf("executed phases = %v, want fail-fast to stop after the first failure", executed)
	}
	if err == nil {
		t.Fatal("expected the verify failure to propagate")
	}
}

func TestRunCIPhasesStopsOnContextCancellationEvenWhenFailFastDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var executed []string
	err := runCIPhases(ctx, []string{"lint", "compile"}, false, func(_ context.Context, phase string) error {
		executed = append(executed, phase)
		if phase == "lint" {
			cancel()
			return context.Canceled
		}
		return nil
	})
	if !reflect.DeepEqual(executed, []string{"lint"}) {
		t.Fatalf("executed phases = %v, want cancellation to stop the gate", executed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := strings.Count(err.Error(), context.Canceled.Error()); got != 1 {
		t.Fatalf("cancellation appears %d times in %q, want it recorded once", got, err.Error())
	}
}

func TestNormalizeRunPhasesDefaultsAndDeduplicates(t *testing.T) {
	got, err := normalizeRunPhases(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sync-drift", "lint", "compile", "test", "audit", "sbom", "build"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default phases = %v, want %v", got, want)
	}

	got, err = normalizeRunPhases([]string{"lint,compile", "lint", "test"})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"lint", "compile", "test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized phases = %v, want %v", got, want)
	}
}

func TestPhaseLocksDependencyClosureOnlyWhenRuntimeResourcesCanOverlap(t *testing.T) {
	for _, phase := range []string{"sync-drift", "test"} {
		if !phaseLocksDependencyClosure(phase) {
			t.Fatalf("phase %q did not lock its dependency closure", phase)
		}
	}
	for _, phase := range []string{"lint", "compile", "audit", "sbom", "build"} {
		if phaseLocksDependencyClosure(phase) {
			t.Fatalf("phase %q unexpectedly locked its dependency closure", phase)
		}
	}
}

func TestNormalizeRunPhasesRejectsProviderOwnedCommandNames(t *testing.T) {
	if _, err := normalizeRunPhases([]string{"go-vet"}); err == nil {
		t.Fatal("language-specific phase was accepted")
	}
}

func TestNormalizeTestSuitesDefaultsAndDeduplicates(t *testing.T) {
	if got := normalizeTestSuites(nil); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("default suites = %v, want agent default", got)
	}
	want := []string{"unit", "integration"}
	if got := normalizeTestSuites([]string{" unit ", "integration", "unit", ""}); !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized suites = %v, want %v", got, want)
	}
}

func TestCIRuntimeAuditIsTheDefault(t *testing.T) {
	flag := RunCmd.Flags().Lookup("audit-include-dev")
	if flag == nil {
		t.Fatal("ci run has no --audit-include-dev flag")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--audit-include-dev default = %q, want false", flag.DefValue)
	}
}

func TestTestCommandSuiteFailurePolicy(t *testing.T) {
	for _, test := range []struct {
		name       string
		failFast   bool
		wantSuites []string
	}{
		{name: "continue", wantSuites: []string{"unit", "integration"}},
		{name: "fail fast", failFast: true, wantSuites: []string{"unit"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _ := loadSchedulerFixture(t)
			t.Chdir(root)
			t.Setenv("CODEFLY_HOME", t.TempDir())
			previousSelection, previousSuites := testSelection, testSuites
			previousContext, previousFailFast := runtimeContext, ciFailFast
			previousOutput, previousFormat := ciReportOutput, ciReportFormat
			t.Cleanup(func() {
				testSelection, testSuites = previousSelection, previousSuites
				runtimeContext, ciFailFast = previousContext, previousFailFast
				ciReportOutput, ciReportFormat = previousOutput, previousFormat
			})
			testSelection = SelectionFlags{changedFiles: []string{"modules/management/services/worker/code/main.go"}}
			testSuites = []string{"unit", "integration"}
			runtimeContext, ciFailFast = "invalid-runtime-context", test.failFast
			ciReportOutput, ciReportFormat = t.TempDir(), "text"
			err := TestCmd.RunE(TestCmd, nil)
			if err == nil || !strings.Contains(err.Error(), "Invalid runtime context") {
				t.Fatalf("error = %v, want runtime configuration failure", err)
			}
			if got := strings.Count(err.Error(), "Invalid runtime context"); got != len(test.wantSuites) {
				t.Fatalf("reported failures = %d, want %d: %v", got, len(test.wantSuites), err)
			}
			payload, readErr := os.ReadFile(filepath.Join(ciReportOutput, reportFilename))
			if readErr != nil {
				t.Fatal(readErr)
			}
			var report CIReport
			if err := json.Unmarshal(payload, &report); err != nil {
				t.Fatal(err)
			}
			var suites []string
			for _, task := range report.Tasks {
				suites = append(suites, task.Suite)
				assertReportTask(t, task, reportStatusFailed, "")
			}
			if !reflect.DeepEqual(suites, test.wantSuites) {
				t.Fatalf("attempted suites = %v, want %v", suites, test.wantSuites)
			}
			if report.Status != reportStatusFailed {
				t.Fatalf("gate status = %s, want failed", report.Status)
			}
		})
	}
}

func TestCITestSuitesStopsOnCancellation(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/worker"}}}
	reporter := fixedCIReporter(t, plan)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := executeCIPhase(ctx, reporter, workspace, plan, "test", []string{"unit", "integration"}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	report := reporter.Finalize(err)
	for _, task := range report.Tasks {
		if task.Suite == "integration" {
			t.Fatal("attempted another suite after cancellation")
		}
	}
}
