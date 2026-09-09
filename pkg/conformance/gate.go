package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	// RequiredEnv lists, comma-separated, the row identifiers a CI job claims
	// to prove. A required row may never skip itself.
	RequiredEnv = "CODEFLY_CONFORMANCE_REQUIRED"
	// ReceiptsEnv names a directory where each gated test drops a receipt, so
	// a job can prove afterwards that the row really ran.
	ReceiptsEnv = "CODEFLY_CONFORMANCE_RECEIPTS"
)

// Receipt is the evidence one gated test leaves behind.
type Receipt struct {
	Row           string            `json:"row"`
	Status        Status            `json:"status"`
	Test          string            `json:"test"`
	Required      bool              `json:"required"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	Go            string            `json:"go"`
	CLI           string            `json:"cli"`
	Core          string            `json:"core"`
	Agents        []string          `json:"agents,omitempty"`
	Prerequisites map[string]string `json:"prerequisites,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	DurationMS    int64             `json:"duration_ms"`
	Passed        bool              `json:"passed"`
}

// Required reports the row identifiers the current process must prove.
func Required() []string {
	raw := strings.Split(os.Getenv(RequiredEnv), ",")
	required := make([]string, 0, len(raw))
	for _, id := range raw {
		if id = strings.TrimSpace(id); id != "" {
			required = append(required, id)
		}
	}
	sort.Strings(required)
	return required
}

// Gate decides whether the calling test may run as evidence for a matrix row.
//
// When a CI job lists the row in CODEFLY_CONFORMANCE_REQUIRED, a missing
// Docker, Nix or cluster prerequisite fails the test. Skipping is reserved for
// rows nobody is currently claiming.
func Gate(t testing.TB, id string) {
	t.Helper()
	gate(t, Default(), id)
}

func gate(t testing.TB, matrix Matrix, id string) {
	t.Helper()
	row, ok := matrix.Row(id)
	if !ok {
		t.Fatalf("conformance: no matrix row %q (see %s)", id, MatrixRelativePath)
	}
	if row.Status == StatusUnsupported {
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
	for _, prerequisite := range row.Prerequisites {
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

	writeReceipt(t, matrix, &row, required, resolved)
}

// requiredRow reports whether this row is claimed, and rejects a CI job that
// claims a row the matrix cannot back.
func requiredRow(t testing.TB, matrix Matrix, id string) bool {
	t.Helper()
	claimed := false
	for _, candidate := range Required() {
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

func writeReceipt(t testing.TB, matrix Matrix, row *Row, required bool, resolved map[string]string) {
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
		name := fmt.Sprintf("%s.%s.json", row.ID, sanitizeTestName(t.Name()))
		if err := os.WriteFile(filepath.Join(directory, name), append(payload, '\n'), 0o600); err != nil { //nolint:gosec // G703: operator-supplied output directory
			t.Errorf("conformance: write receipt for %s: %v", row.ID, err)
		}
	})
}

func sanitizeTestName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
}
