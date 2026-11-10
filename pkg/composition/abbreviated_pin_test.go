package composition

import "testing"

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
