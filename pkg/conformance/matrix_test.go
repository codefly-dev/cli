package conformance

import (
	"encoding/json"
	"strings"
	"testing"
)

// validRow is the minimal row every rejection case below mutates.
func validRow() Row {
	return Row{
		ID: "linux-amd64-example", Status: StatusQualified, Summary: "example",
		OS: "linux", Arch: "amd64", Backend: "native",
		Prerequisites: []string{"npm"}, CI: []string{"go.yml#coverage"},
		Gates: []string{"pkg/example"},
	}
}

func validMatrix(rows ...Row) Matrix {
	return Matrix{SchemaVersion: 1, CLI: "source", Core: "v0.3.24", Rows: rows}
}

func TestValidateAcceptsAWellFormedMatrix(t *testing.T) {
	if err := validMatrix(validRow()).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		matrix Matrix
		want   string
	}{
		{
			name:   "a qualified row with no CI job",
			matrix: func() Matrix { r := validRow(); r.CI = nil; return validMatrix(r) }(),
			want:   "names no CI job",
		},
		{
			name: "a qualified row that still lists blockers",
			matrix: func() Matrix {
				r := validRow()
				r.Blockers = []string{"nothing runs it"}
				return validMatrix(r)
			}(),
			want: "still lists blockers",
		},
		{
			name: "a not-yet-qualified row claiming a CI job",
			matrix: func() Matrix {
				r := validRow()
				r.Status = StatusNotYetQualified
				return validMatrix(r)
			}(),
			want: "names a CI job but is not qualified",
		},
		{
			name: "a not-yet-qualified row that does not say what blocks it",
			matrix: func() Matrix {
				r := validRow()
				r.Status, r.CI = StatusNotYetQualified, nil
				return validMatrix(r)
			}(),
			want: "must say what blocks it",
		},
		{
			name: "a qualified cluster row without a pinned kubernetes version",
			matrix: func() Matrix {
				r := validRow()
				r.Backend = "k3d"
				return validMatrix(r)
			}(),
			want: "must pin a kubernetes version",
		},
		{
			name: "an unsupported row carrying evidence",
			matrix: func() Matrix {
				r := validRow()
				r.Status = StatusUnsupported
				return validMatrix(r)
			}(),
			want: "must carry no evidence",
		},
		{
			name: "prerequisites no gate call site enforces",
			matrix: func() Matrix {
				r := validRow()
				r.Gates = nil
				return validMatrix(r)
			}(),
			want: "no gate call site enforces them",
		},
		{
			name: "an opt-in switch no gate call site reads",
			matrix: func() Matrix {
				r := validRow()
				r.Prerequisites, r.Gates, r.GateEnv = nil, nil, "CODEFLY_EXAMPLE_QUALIFY"
				return validMatrix(r)
			}(),
			want: "no gate call site reads",
		},
		{
			name: "two rows sharing one opt-in switch",
			matrix: func() Matrix {
				first, second := validRow(), validRow()
				first.GateEnv, second.GateEnv = "CODEFLY_EXAMPLE_QUALIFY", "CODEFLY_EXAMPLE_QUALIFY"
				second.ID = "linux-amd64-other"
				return validMatrix(first, second)
			}(),
			want: "share gate_env",
		},
		{
			name:   "a repeated row identifier",
			matrix: validMatrix(validRow(), validRow()),
			want:   "repeats row",
		},
		{
			name: "a kubernetes version without a cluster backend",
			matrix: func() Matrix {
				r := validRow()
				r.Kubernetes = "v1.31.5"
				return validMatrix(r)
			}(),
			want: "without a cluster backend",
		},
		{
			name: "an inexact agent pin",
			matrix: func() Matrix {
				r := validRow()
				r.Agents = []Agent{{Publisher: "codefly.dev", Name: "redis", Version: "v0.0.74"}}
				return validMatrix(r)
			}(),
			want: "invalid exact version",
		},
		{
			name: "a core pin that is not a version",
			matrix: func() Matrix {
				m := validMatrix(validRow())
				m.Core = "latest"
				return m
			}(),
			want: "want a vX.Y.Z pin",
		},
		{
			name:   "an unknown schema version",
			matrix: func() Matrix { m := validMatrix(validRow()); m.SchemaVersion = 2; return m }(),
			want:   "schema_version",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.matrix.Validate()
			if err == nil {
				t.Fatalf("matrix was accepted, want rejection mentioning %q", test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("rejection = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsAnInvalidDocument(t *testing.T) {
	payload, err := json.Marshal(validMatrix(func() Row { r := validRow(); r.CI = nil; return r }()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(payload); err == nil {
		t.Fatal("Parse accepted a matrix Validate rejects")
	}
}

// TestRequirableExcludesRowsWithNoGate keeps a CI job from claiming a row that
// nothing can enforce.
func TestRequirableExcludesRowsWithNoGate(t *testing.T) {
	row := validRow()
	row.Prerequisites, row.Gates = nil, nil
	if row.Requirable() {
		t.Fatal("a row with no gate call site is requirable")
	}
	gated := validRow()
	if !gated.Requirable() {
		t.Fatal("a gated row is not requirable")
	}
}

// TestEmbeddedMatrixIsQualifiedOnlyWhereCIProvesIt documents the shipped claim,
// so widening it is a deliberate edit rather than a side effect.
func TestEmbeddedMatrixIsQualifiedOnlyWhereCIProvesIt(t *testing.T) {
	qualified := map[string]bool{}
	for _, row := range Default().Rows {
		if row.Status == StatusQualified {
			qualified[row.ID] = true
		}
	}
	want := map[string]bool{
		"linux-amd64-source":     true,
		"linux-amd64-native-npm": true,
		"linux-amd64-nix-run":    true,
	}
	if len(qualified) != len(want) {
		t.Fatalf("qualified rows = %v, want %v", qualified, want)
	}
	for id := range want {
		if !qualified[id] {
			t.Errorf("row %s is no longer qualified", id)
		}
	}
}
