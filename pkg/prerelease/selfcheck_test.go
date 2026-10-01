package prerelease

import (
	"testing"
)

// TestThisRepositoryPassesItsOwnGate runs the gate over the CLI repository on
// every `go test ./...`, so the repository that implements the rule is held to it
// from the day it lands.
//
// Deliberately a test rather than a new workflow job: it rides the gates that are
// already required on every pull request and in the merge queue, so it needs no
// branch-protection change to be enforced — and a required check that has to be
// added to a protected branch before it bites is a gate that does not bite yet.
//
// Other repositories call `codefly ci prerelease` from their own CI; see
// docs/prerelease-gate.md for the step.
func TestThisRepositoryPassesItsOwnGate(t *testing.T) {
	result, err := Scan("../..", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() {
		t.Errorf("this repository carries a prerelease version pin:\n%s", result.Report())
	}
	// The scan found the repository, rather than passing because it read nothing.
	if len(result.Files) == 0 {
		t.Fatal("scanned no files, so the check above passes vacuously")
	}
	if !result.Tracked {
		t.Error("the repository root is a git work tree; the scan should have read what git tracks")
	}
	for _, finding := range result.Allowed() {
		t.Logf("permitted: %s  %s = %s", finding.Location(), finding.Key, finding.Version)
	}
}
