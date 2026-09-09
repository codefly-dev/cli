package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
func runGate(t *testing.T, matrix Matrix, id string) *recordingTB {
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
		gate(recorder, matrix, id)
	}()
	for _, cleanup := range recorder.cleanups {
		cleanup()
	}
	return recorder
}

// hostMatrix builds a matrix whose rows target the running host, so the tests
// exercise the decision table rather than the CI runner's platform.
func hostMatrix(rows ...Row) Matrix {
	for i := range rows {
		rows[i].OS = runtime.GOOS
		rows[i].Arch = runtime.GOARCH
	}
	return Matrix{SchemaVersion: 1, CLI: "source", Core: "v0.0.1", Rows: rows}
}

func gatedRow() Row {
	return Row{
		ID: "host-gated-row", Status: StatusNotYetQualified, Summary: "gated",
		Backend: "docker", Prerequisites: []string{"codefly-absent-binary"},
		GateEnv: "CODEFLY_TEST_QUALIFY", Gates: []string{"pkg/conformance"},
		Blockers: []string{"test fixture"},
	}
}

func openRow() Row {
	return Row{
		ID: "host-open-row", Status: StatusQualified, Summary: "open",
		Backend: "native", Prerequisites: []string{"codefly-absent-binary"},
		CI: []string{"go.yml#coverage"}, Gates: []string{"pkg/conformance"},
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
	row := gatedRow()
	matrix := hostMatrix(row)
	matrix.Rows[0].OS = "plan9"
	decision := runGate(t, matrix, "host-gated-row")
	if !strings.Contains(decision.fatal, "plan9") {
		t.Fatalf("failure does not name the row's platform: %q", decision.fatal)
	}
}

func TestGateRejectsClaimNoGateEnforces(t *testing.T) {
	t.Setenv(RequiredEnv, "host-ungated-row")
	matrix := hostMatrix(gatedRow(), Row{
		ID: "host-ungated-row", Status: StatusQualified, Summary: "no gate",
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
	row.Agents = []Agent{{Publisher: "codefly.dev", Name: "redis", Version: "0.0.74"}}

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
