package composition

import (
	"strings"
	"testing"
)

// The pin spelling that resolves nowhere. Git resolves an abbreviated SHA
// locally and a remote does not, so every clone reads it as a ref name and the
// render fails with "Remote branch <pin> not found in upstream origin" — naming a
// branch nobody wrote. These cases fix which spellings are that case.
func TestAbbreviatedCommitPin(t *testing.T) {
	full := "e55299311a8ae55299311a8ae55299311a8ae552"
	for _, tc := range []struct {
		name    string
		version string
		want    bool
	}{
		{"the 12-character SHA that cost a render on 2026-11-10", "e55299311a8a", true},
		{"git's shortest abbreviation", "e552", true},
		{"one character short of the object name", full[:39], true},
		{"uppercase hex, which git also resolves locally", "E55299311A8A", true},
		{"the full object name is a commit pin, not this", full, false},
		{"a semver tag", "0.2.2", false},
		{"a v-prefixed tag", "v0.2.2", false},
		{"a range", ">=0.1.225 <1.0.0", false},
		{"a branch name", "main", false},
		{"empty, which means latest", "", false},
		{"shorter than git will abbreviate, so not diagnosed as one", "e5", false},
		{"not hex", "zzzzzzzz", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AbbreviatedCommitPin(tc.version); got != tc.want {
				t.Fatalf("AbbreviatedCommitPin(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

// A commit pin and an abbreviated one are disjoint: the full object name is
// never reported as the near miss, and the near miss is never a commit pin.
func TestAbbreviatedAndCommitPinAreDisjoint(t *testing.T) {
	full := "e55299311a8ae55299311a8ae55299311a8ae552"
	if !IsCommitPin(full) || AbbreviatedCommitPin(full) {
		t.Fatal("the full object name is a commit pin and not an abbreviation")
	}
	if IsCommitPin(full[:12]) || !AbbreviatedCommitPin(full[:12]) {
		t.Fatal("a 12-character SHA is an abbreviation and not a commit pin")
	}
}

// A commit pin is lowercase, as `git rev-parse` prints it. The full object name
// pasted from a UI that renders it uppercase is the near miss most likely to be a
// copy-paste, and it is neither a commit pin nor an abbreviation.
func TestUppercaseCommitPin(t *testing.T) {
	lower := "e55299311a8ae55299311a8ae55299311a8ae552"
	upper := "E55299311A8AE55299311A8AE55299311A8AE552"
	if UppercaseCommitPin(lower) {
		t.Fatal("the lowercase object name is a commit pin, not the uppercase near miss")
	}
	if !UppercaseCommitPin(upper) {
		t.Fatal("the uppercase object name must be recognised as the wrong case")
	}
	if IsCommitPin(upper) {
		t.Fatal("uppercase is not a commit pin")
	}
	if AbbreviatedCommitPin(upper) {
		t.Fatal("a 40-character pin is not an abbreviation, whatever its case")
	}
	for _, other := range []string{"v0.2.2", "main", "", ">=1.0.0", lower[:12]} {
		if UppercaseCommitPin(other) {
			t.Fatalf("UppercaseCommitPin(%q) must be false", other)
		}
	}
}

// One predicate answers every spelling that cannot resolve, because the first
// version derived the question twice and the two answers disagreed: a hex-looking
// tag was protected in one place and refused in the other, and a pin hold could
// still carry the spelling the other guard had not learned.
func TestUnresolvablePin(t *testing.T) {
	lower := "e55299311a8ae55299311a8ae55299311a8ae552"
	upper := "E55299311A8AE55299311A8AE55299311A8AE552"
	mixed := "E55299311a8ae55299311a8ae55299311a8ae552"

	for _, tc := range []struct {
		name    string
		version string
		tags    []string
		bad     bool
		says    string
	}{
		{"the 12-character SHA from 2026-11-10", lower[:12], []string{"v0.2.2"}, true, "abbreviated commit SHA"},
		{"a tagless source still judges the shape", lower[:12], nil, true, "abbreviated commit SHA"},
		{"a published tag that happens to be hex is a tag", "deadbeef", []string{"deadbeef"}, false, ""},
		{"the same spelling unpublished is a near miss", "deadbeef", []string{"v1.0.0"}, true, "abbreviated commit SHA"},
		{"the object name in uppercase", upper, []string{"v1.0.0"}, true, "lowercase spelling"},
		{"a published 40-hex uppercase TAG is a tag, not a near miss", upper, []string{upper}, false, ""},
		{"mixed case is the same defect and must not be called uppercase", mixed, nil, true, "lowercase spelling"},
		{"the lowercase object name resolves", lower, nil, false, ""},
		{"a semver tag", "v0.2.2", nil, false, ""},
		{"a range", ">=0.1.225 <1.0.0", nil, false, ""},
		{"a branch name", "main", nil, false, ""},
		{"empty means latest", "", nil, false, ""},
		{"padding is trimmed", "  " + lower[:12] + "  ", nil, true, "abbreviated commit SHA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, bad := UnresolvablePin(tc.version, tc.tags)
			if bad != tc.bad {
				t.Fatalf("UnresolvablePin(%q, %v) = %v, want %v (detail %q)", tc.version, tc.tags, bad, tc.bad, detail)
			}
			if tc.says != "" && !strings.Contains(detail, tc.says) {
				t.Fatalf("detail %q does not name %q", detail, tc.says)
			}
			if !bad && detail != "" {
				t.Fatalf("a resolvable pin carries no detail, got %q", detail)
			}
		})
	}
}
