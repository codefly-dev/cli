package ci

import (
	"reflect"
	"testing"
)

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
