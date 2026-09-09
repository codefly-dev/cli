package conformancetest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/conformance"
)

// TestSourceLaneIsExercised is the linux-amd64-source row's gate call site.
// The row claims the fast source lane runs on every change; this is what
// leaves the receipt proving it did.
func TestSourceLaneIsExercised(t *testing.T) {
	Gate(t, "linux-amd64-source")
}

// recordingTB captures a gate decision. Fatalf and Skipf abort the caller the
// way the real ones do, so the code under test cannot run past them.
type recordingTB struct {
	testing.TB
	name     string
	fatal    string
	skip     string
	cleanups []func()
}

type gateStopped struct{}

func (r *recordingTB) Helper()      {}
func (r *recordingTB) Name() string { return r.name }
func (r *recordingTB) Failed() bool { return false }
func (r *recordingTB) Cleanup(fn func()) {
	r.cleanups = append(r.cleanups, fn)
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	panic(gateStopped{})
}

func (r *recordingTB) Skipf(format string, args ...any) {
	r.skip = fmt.Sprintf(format, args...)
	panic(gateStopped{})
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

// runGate drives the gate against a synthetic matrix and reports what it did.
func runGate(t *testing.T, matrix conformance.Matrix, id string, needs ...string) *recordingTB {
	t.Helper()
	recorder := &recordingTB{name: t.Name()}
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if _, stopped := recovered.(gateStopped); !stopped {
					panic(recovered)
				}
			}
		}()
		gate(recorder, matrix, "pkg/fixture", id, needs)
	}()
	for _, cleanup := range recorder.cleanups {
		cleanup()
	}
	return recorder
}

// hostMatrix builds a matrix whose rows target the running host, so the tests
// exercise the decision table rather than the CI runner's platform.
func hostMatrix(rows ...conformance.Row) conformance.Matrix {
	for i := range rows {
		rows[i].OS = runtime.GOOS
		rows[i].Arch = runtime.GOARCH
	}
	return conformance.Matrix{SchemaVersion: 1, CLI: "source", Core: "v0.0.1", Rows: rows}
}

func gatedRow() conformance.Row {
	return conformance.Row{
		ID: "host-gated-row", Status: conformance.StatusNotYetQualified, Summary: "gated",
		Backend: "docker", Prerequisites: []string{"codefly-absent-binary"},
		GateEnv: "CODEFLY_TEST_QUALIFY", Gates: []string{"pkg/conformance/conformancetest"},
		Blockers: []string{"test fixture"},
	}
}

func openRow() conformance.Row {
	return conformance.Row{
		ID: "host-open-row", Status: conformance.StatusQualified, Summary: "open",
		Backend: "native", Prerequisites: []string{"codefly-absent-binary"},
		CI: []string{"go.yml#coverage"}, Gates: []string{"pkg/conformance/conformancetest"},
	}
}

// emptyPath removes every prerequisite from the host's PATH.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestGateSkipsRowNobodyClaims(t *testing.T) {
	t.Setenv(RequiredEnv, "")
	emptyPath(t)
	decision := runGate(t, hostMatrix(gatedRow()), "host-gated-row")
	if decision.fatal != "" {
		t.Fatalf("unclaimed row failed: %s", decision.fatal)
	}
	if !strings.Contains(decision.skip, "CODEFLY_TEST_QUALIFY") {
		t.Fatalf("skip does not say how to enable the row: %q", decision.skip)
	}
}

// TestGateFailsRequiredRowMissingPrerequisite is the contract this package
// exists for: a claimed row may not skip its way to green.
func TestGateFailsRequiredRowMissingPrerequisite(t *testing.T) {
	t.Setenv(RequiredEnv, "host-gated-row")
	emptyPath(t)
	decision := runGate(t, hostMatrix(gatedRow()), "host-gated-row")
	if decision.skip != "" {
		t.Fatalf("required row skipped: %s", decision.skip)
	}
	if !strings.Contains(decision.fatal, "codefly-absent-binary") {
		t.Fatalf("failure does not name the missing prerequisite: %q", decision.fatal)
	}
}

func TestGateFailsOptedInRowMissingPrerequisite(t *testing.T) {
	t.Setenv(RequiredEnv, "")
	t.Setenv("CODEFLY_TEST_QUALIFY", "1")
	emptyPath(t)
	decision := runGate(t, hostMatrix(gatedRow()), "host-gated-row")
	if decision.skip != "" {
		t.Fatalf("opted-in row skipped: %s", decision.skip)
	}
	if !strings.Contains(decision.fatal, "CODEFLY_TEST_QUALIFY") {
		t.Fatalf("failure does not name the opt-in: %q", decision.fatal)
	}
}

// A row's prerequisites are the union over its tests, so a caller that names
// the subset it uses must not be blocked by a tool it never invokes. Without
// this, adding buf to the docker-generate row stopped cmd/generate's tests —
// which never run buf — from qualifying on a host that has Docker only.
func TestGateNarrowsToTheNeedsACallerNames(t *testing.T) {
	t.Setenv(RequiredEnv, "")
	t.Setenv("CODEFLY_TEST_QUALIFY", "1")
	row := gatedRow()
	row.Prerequisites = []string{"sh", "codefly-absent-binary"}

	decision := runGate(t, hostMatrix(row), "host-gated-row", "sh")
	if decision.fatal != "" || decision.skip != "" {
		t.Fatalf("narrowed gate did not admit the test: fatal=%q skip=%q", decision.fatal, decision.skip)
	}
}

// Narrowing is a local convenience, never a way to weaken a CI claim: a
// claimed row is held to everything it declares.
func TestGateHoldsAClaimedRowToEveryPrerequisite(t *testing.T) {
	t.Setenv(RequiredEnv, "host-gated-row")
	row := gatedRow()
	row.Prerequisites = []string{"sh", "codefly-absent-binary"}

	decision := runGate(t, hostMatrix(row), "host-gated-row", "sh")
	if !strings.Contains(decision.fatal, "codefly-absent-binary") {
		t.Fatalf("claimed row was narrowed to %q: fatal=%q skip=%q", "sh", decision.fatal, decision.skip)
	}
}

func TestGateRejectsANeedTheRowDoesNotDeclare(t *testing.T) {
	t.Setenv(RequiredEnv, "")
	t.Setenv("CODEFLY_TEST_QUALIFY", "1")
	decision := runGate(t, hostMatrix(gatedRow()), "host-gated-row", "kubectl")
	if !strings.Contains(decision.fatal, "does not declare prerequisite") {
		t.Fatalf("undeclared need was accepted: %q", decision.fatal)
	}
}

// A row with no opt-in switch runs opportunistically, so a developer without
// the tooling is not blocked by a row CI is not currently claiming.
func TestGateSkipsUnclaimedRowWithoutOptInSwitch(t *testing.T) {
	t.Setenv(RequiredEnv, "")
	emptyPath(t)
	decision := runGate(t, hostMatrix(openRow()), "host-open-row")
	if decision.fatal != "" {
		t.Fatalf("unclaimed row failed: %s", decision.fatal)
	}
	if !strings.Contains(decision.skip, "codefly-absent-binary") {
		t.Fatalf("skip does not name the prerequisite: %q", decision.skip)
	}
}

func TestGateFailsRequiredRowOnForeignPlatform(t *testing.T) {
	t.Setenv(RequiredEnv, "host-gated-row")
	matrix := hostMatrix(gatedRow())
	matrix.Rows[0].OS = "plan9"
	decision := runGate(t, matrix, "host-gated-row")
	if !strings.Contains(decision.fatal, "plan9") {
		t.Fatalf("failure does not name the row's platform: %q", decision.fatal)
	}
}

func TestGateRejectsClaimNoGateEnforces(t *testing.T) {
	t.Setenv(RequiredEnv, "host-ungated-row")
	matrix := hostMatrix(gatedRow(), conformance.Row{
		ID: "host-ungated-row", Status: conformance.StatusQualified, Summary: "no gate",
		Backend: "none", CI: []string{"go.yml#coverage"},
	})
	decision := runGate(t, matrix, "host-gated-row")
	if !strings.Contains(decision.fatal, "no gate call site") {
		t.Fatalf("claim on an unenforceable row was accepted: %q", decision.fatal)
	}
}

func TestGateRejectsUnknownRow(t *testing.T) {
	t.Setenv(RequiredEnv, "")
	decision := runGate(t, hostMatrix(gatedRow()), "no-such-row")
	if !strings.Contains(decision.fatal, "no matrix row") {
		t.Fatalf("unknown row was accepted: %q", decision.fatal)
	}
}

func TestGateWritesReceiptForAnAdmittedRow(t *testing.T) {
	receipts := t.TempDir()
	t.Setenv(RequiredEnv, "host-open-row")
	t.Setenv(ReceiptsEnv, receipts)
	row := openRow()
	row.Prerequisites = nil
	row.Agents = []conformance.Agent{{Publisher: "codefly.dev", Name: "redis", Version: "0.0.74"}}

	decision := runGate(t, hostMatrix(row), "host-open-row")
	if decision.fatal != "" || decision.skip != "" {
		t.Fatalf("admitted row did not run: fatal=%q skip=%q", decision.fatal, decision.skip)
	}

	entries, err := filepath.Glob(filepath.Join(receipts, "host-open-row.*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("receipts = %v (%v), want exactly one", entries, err)
	}
	payload, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Row != "host-open-row" || !receipt.Required || !receipt.Passed {
		t.Fatalf("receipt = %+v", receipt)
	}
	if len(receipt.Agents) != 1 || receipt.Agents[0] != "codefly.dev/redis:0.0.74" {
		t.Fatalf("receipt agents = %v", receipt.Agents)
	}
}

// Every package gating one row writes into the same directory, and two
// packages may hold a same-named test. Without the package in the name their
// receipts are the same path, and concurrent package binaries truncate each
// other's evidence.
func TestReceiptNamesAreScopedToThePackage(t *testing.T) {
	receipts := t.TempDir()
	t.Setenv(RequiredEnv, "host-open-row")
	t.Setenv(ReceiptsEnv, receipts)
	row := openRow()
	row.Prerequisites = nil
	matrix := hostMatrix(row)

	recorder := &recordingTB{name: "TestSameName"}
	gate(recorder, matrix, "pkg/first", "host-open-row", nil)
	gate(recorder, matrix, "cmd/second", "host-open-row", nil)
	for _, cleanup := range recorder.cleanups {
		cleanup()
	}

	entries, err := filepath.Glob(filepath.Join(receipts, "host-open-row.*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("same-named tests in two packages left %d receipt(s), want 2: %v", len(entries), entries)
	}
}

// Gate must attribute a receipt to the package that called it, not to the
// harness. The write happens in a cleanup, so the assertion lives outside the
// subtest whose cleanups it waits on.
func TestGateAttributesTheReceiptToTheCallingPackage(t *testing.T) {
	receipts := t.TempDir()
	t.Setenv(RequiredEnv, "")
	t.Setenv(ReceiptsEnv, receipts)

	t.Run("gated", func(t *testing.T) { Gate(t, "linux-amd64-source") })

	matches, err := filepath.Glob(filepath.Join(receipts, "linux-amd64-source.*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("receipts = %v (%v), want exactly one", matches, err)
	}
	payload, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(payload, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Package != "github.com/codefly-dev/cli/pkg/conformance/conformancetest" {
		t.Fatalf("receipt package = %q, want the calling package", receipt.Package)
	}
}

func TestGateWritesNoReceiptForASkippedRow(t *testing.T) {
	receipts := t.TempDir()
	t.Setenv(RequiredEnv, "")
	t.Setenv(ReceiptsEnv, receipts)
	emptyPath(t)

	runGate(t, hostMatrix(gatedRow()), "host-gated-row")

	entries, err := os.ReadDir(receipts)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("skipped row left %d receipt(s)", len(entries))
	}
}
