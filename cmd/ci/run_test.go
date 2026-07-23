package ci

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRunCIPhasesContinuesAfterFailureWhenFailFastDisabled(t *testing.T) {
	phases := []string{"verify", "lint", "compile", "test"}
	var executed []string
	err := runCIPhases(context.Background(), phases, false, func(_ context.Context, phase string) error {
		executed = append(executed, phase)
		if phase == "verify" {
			return errors.New("workspace verification failed")
		}
		return nil
	})
	if !reflect.DeepEqual(executed, phases) {
		t.Fatalf("executed phases = %v, want every phase to run despite early failure", executed)
	}
	if err == nil || !strings.Contains(err.Error(), "CI phase verify failed") {
		t.Fatalf("error = %v, want the verify failure to be reported", err)
	}
}

func TestRunCIPhasesAggregatesEveryPhaseFailureWhenFailFastDisabled(t *testing.T) {
	err := runCIPhases(context.Background(), []string{"verify", "lint", "compile"}, false, func(_ context.Context, phase string) error {
		if phase == "verify" || phase == "compile" {
			return errors.New(phase + " broke")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected accumulated failures")
	}
	if !strings.Contains(err.Error(), "verify broke") || !strings.Contains(err.Error(), "compile broke") {
		t.Fatalf("error = %v, want both independent failures reported", err)
	}
}

func TestRunCIPhasesStopsAtFirstFailureWhenFailFastEnabled(t *testing.T) {
	var executed []string
	err := runCIPhases(context.Background(), []string{"verify", "lint", "compile"}, true, func(_ context.Context, phase string) error {
		executed = append(executed, phase)
		if phase == "verify" {
			return errors.New("workspace verification failed")
		}
		return nil
	})
	if !reflect.DeepEqual(executed, []string{"verify"}) {
		t.Fatalf("executed phases = %v, want fail-fast to stop after the first failure", executed)
	}
	if err == nil {
		t.Fatal("expected the verify failure to propagate")
	}
}

func TestRunCIPhasesStopsOnContextCancellationEvenWhenFailFastDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var executed []string
	err := runCIPhases(ctx, []string{"verify", "lint", "compile"}, false, func(_ context.Context, phase string) error {
		executed = append(executed, phase)
		if phase == "verify" {
			cancel()
			return context.Canceled
		}
		return nil
	})
	if !reflect.DeepEqual(executed, []string{"verify"}) {
		t.Fatalf("executed phases = %v, want cancellation to stop the gate", executed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestNormalizeRunPhasesDefaultsAndDeduplicates(t *testing.T) {
	got, err := normalizeRunPhases(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"verify", "sync-drift", "lint", "compile", "test", "audit", "sbom", "build"}
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
	for _, phase := range []string{"verify", "lint", "compile", "audit", "sbom", "build"} {
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
