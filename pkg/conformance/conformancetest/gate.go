// Package conformancetest admits a test as evidence for a conformance matrix
// row, and records the receipt proving the row ran.
//
// It is separate from pkg/conformance so that the matrix — the published claim
// — stays importable by ordinary code without dragging in testing.
package conformancetest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/conformance"
)

const (
	RequiredEnv = conformance.RequiredEnv
	ReceiptsEnv = conformance.ReceiptsEnv
)

// Receipt is the evidence one gated test leaves behind.
type Receipt struct {
	Row           string             `json:"row"`
	Status        conformance.Status `json:"status"`
	Package       string             `json:"package"`
	Test          string             `json:"test"`
	Required      bool               `json:"required"`
	OS            string             `json:"os"`
	Arch          string             `json:"arch"`
	Go            string             `json:"go"`
	CLI           string             `json:"cli"`
	Core          string             `json:"core"`
	Agents        []string           `json:"agents,omitempty"`
	Prerequisites map[string]string  `json:"prerequisites,omitempty"`
	StartedAt     time.Time          `json:"started_at"`
	DurationMS    int64              `json:"duration_ms"`
	Passed        bool               `json:"passed"`
}

// requiredRows reports the row identifiers the current process must prove.
func requiredRows() []string {
	var required []string
	for _, id := range strings.Split(os.Getenv(RequiredEnv), ",") {
		if id = strings.TrimSpace(id); id != "" {
			required = append(required, id)
		}
	}
	return required
}

// Gate decides whether the calling test may run as evidence for a matrix row.
//
// When a CI job lists the row in CODEFLY_CONFORMANCE_REQUIRED, every
// prerequisite the row declares must be present: a missing Docker, Nix or
// cluster fails the test rather than skipping it. Outside that claim, a caller
// may narrow the check to the subset it actually uses by naming those
// prerequisites in needs, so a partial local toolchain still qualifies the
// tests it can run.
func Gate(t testing.TB, id string, needs ...string) {
	t.Helper()
	gate(t, conformance.Default(), callerPackage(), id, needs)
}

func gate(t testing.TB, matrix conformance.Matrix, pkg, id string, needs []string) {
	t.Helper()
	row, ok := matrix.Row(id)
	if !ok {
		t.Fatalf("conformance: no matrix row %q (see %s)", id, conformance.MatrixRelativePath)
	}
	if row.Status == conformance.StatusUnsupported {
		t.Fatalf("conformance: row %s is unsupported and must not gate a test", id)
	}
	required := requiredRow(t, matrix, id)

	// A CI job that claims a row must run it on the platform the row names.
	// A developer opting in locally is trusted to know what they are on.
	if required && (runtime.GOOS != row.OS || runtime.GOARCH != row.Arch) {
		t.Fatalf("conformance: row %s is required by %s but declares %s/%s, and this host is %s/%s",
			id, RequiredEnv, row.OS, row.Arch, runtime.GOOS, runtime.GOARCH)
	}

	if !required && row.GateEnv != "" && os.Getenv(row.GateEnv) != "1" {
		t.Skipf("conformance: row %s is not enabled; set %s=1, or list it in %s to require it",
			id, row.GateEnv, RequiredEnv)
	}

	resolved := map[string]string{}
	for _, prerequisite := range prerequisitesFor(t, &row, required, needs) {
		path, err := exec.LookPath(prerequisite)
		if err == nil {
			resolved[prerequisite] = path
			continue
		}
		// An opted-in or required row asked for this backend, so a missing
		// prerequisite is a failure. Only an unclaimed row may step aside.
		if required {
			t.Fatalf("conformance: row %s is required by %s but %s is not on PATH: %v",
				id, RequiredEnv, prerequisite, err)
		}
		if row.GateEnv != "" {
			t.Fatalf("conformance: row %s was enabled with %s=1 but %s is not on PATH: %v",
				id, row.GateEnv, prerequisite, err)
		}
		t.Skipf("conformance: row %s needs %s on PATH: %v", id, prerequisite, err)
	}

	writeReceipt(t, matrix, &row, pkg, required, resolved)
}

// prerequisitesFor resolves which of the row's prerequisites this call must
// find. A claimed row is held to everything it declares — otherwise a runner
// missing a tool could still report the row qualified. An unclaimed caller is
// held only to what it named, because a row's prerequisites are the union over
// its tests and no single test needs all of them.
func prerequisitesFor(t testing.TB, row *conformance.Row, required bool, needs []string) []string {
	t.Helper()
	declared := map[string]bool{}
	for _, prerequisite := range row.Prerequisites {
		declared[prerequisite] = true
	}
	for _, need := range needs {
		if !declared[need] {
			t.Fatalf("conformance: row %s does not declare prerequisite %q; add it to the row or drop it here",
				row.ID, need)
		}
	}
	if required || len(needs) == 0 {
		return row.Prerequisites
	}
	return needs
}

// requiredRow reports whether this row is claimed, and rejects a CI job that
// claims a row the matrix cannot back.
func requiredRow(t testing.TB, matrix conformance.Matrix, id string) bool {
	t.Helper()
	claimed := false
	for _, candidate := range requiredRows() {
		row, ok := matrix.Row(candidate)
		if !ok {
			t.Fatalf("conformance: %s names unknown row %q", RequiredEnv, candidate)
		}
		if !row.Requirable() {
			t.Fatalf("conformance: %s names row %q, which has no gate call site to enforce it", RequiredEnv, candidate)
		}
		if candidate == id {
			claimed = true
		}
	}
	return claimed
}

// callerPackage is the import path of the package that called Gate. It
// disambiguates receipts: every package gating one row writes into the same
// directory, and two packages may hold a same-named test.
func callerPackage() string {
	pc, _, _, ok := runtime.Caller(2)
	if !ok {
		return "unknown"
	}
	name := runtime.FuncForPC(pc).Name()
	// "import/path.Func" or "import/path.(*Type).Method" — the package path
	// ends at the last "/" segment's first ".".
	slash := strings.LastIndex(name, "/")
	dot := strings.Index(name[slash+1:], ".")
	if dot < 0 {
		return name
	}
	return name[:slash+1+dot]
}

func writeReceipt(t testing.TB, matrix conformance.Matrix, row *conformance.Row, pkg string, required bool, resolved map[string]string) {
	t.Helper()
	directory := os.Getenv(ReceiptsEnv)
	if directory == "" {
		return
	}
	// The receipts directory is CI configuration, not request input.
	if err := os.MkdirAll(directory, 0o750); err != nil { //nolint:gosec // G703: operator-supplied output directory
		t.Fatalf("conformance: create receipts directory %s: %v", directory, err)
	}
	agents := make([]string, 0, len(row.Agents))
	for i := range row.Agents {
		agents = append(agents, row.Agents[i].Identifier())
	}
	receipt := Receipt{
		Row:           row.ID,
		Status:        row.Status,
		Package:       pkg,
		Test:          t.Name(),
		Required:      required,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		Go:            runtime.Version(),
		CLI:           matrix.CLI,
		Core:          matrix.Core,
		Agents:        agents,
		Prerequisites: resolved,
		StartedAt:     time.Now().UTC(),
	}
	started := time.Now()
	t.Cleanup(func() {
		receipt.DurationMS = time.Since(started).Milliseconds()
		receipt.Passed = !t.Failed()
		payload, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			t.Errorf("conformance: encode receipt for %s: %v", row.ID, err)
			return
		}
		name := fmt.Sprintf("%s.%s.%s.json", row.ID, sanitize(pkg), sanitize(t.Name()))
		if err := os.WriteFile(filepath.Join(directory, name), append(payload, '\n'), 0o600); err != nil { //nolint:gosec // G703: operator-supplied output directory
			t.Errorf("conformance: write receipt for %s: %v", row.ID, err)
		}
	})
}

func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
}
