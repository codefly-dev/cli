package usagelint

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	usagecontract "github.com/codefly-dev/core/contracts/usage"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

// The contract an agent would advertise: core's own accepted fixture, so
// this package is held to a document the one reader accepts.
func advertised(t *testing.T) string {
	t.Helper()
	fixtures, err := usagecontract.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		if fixture.Name == "full" {
			return string(fixture.Document)
		}
	}
	t.Fatal("core ships no full usage contract fixture")
	return ""
}

func consumer(state basev0.SemanticIndexState) *basev0.SemanticIndex {
	at := func(path string, line int32) *basev0.SemanticLocation {
		return &basev0.SemanticLocation{Path: path, StartLine: line, StartColumn: 9}
	}
	return &basev0.SemanticIndex{
		State:              state,
		FileCallsProjected: true,
		Languages:          []string{"go"},
		Files: []*basev0.SemanticFile{
			{Path: "store/open.go", Language: "go", Imports: []string{"database/sql"}},
			{Path: "store/clean.go", Language: "go", Imports: []string{"context"}},
		},
		Symbols: []*basev0.SemanticSymbol{{
			Name: "Open", Location: at("store/open.go", 8),
			Calls: []*basev0.SemanticUse{{Name: "sql.Open", Location: at("store/open.go", 12)}},
		}},
	}
}

func TestCheckReportsAViolationInTheOwnersWords(t *testing.T) {
	sources := []Source{{Owner: "codefly.dev/store:1.5.0", Through: "dependency backend/store", Document: advertised(t)}}
	report, err := Check("backend/api", sources, consumer(basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE))
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 1 || report.Warnings != 0 || len(report.Findings) != 1 || len(report.Incomplete) != 0 {
		t.Fatalf("got %+v", report)
	}
	finding := report.Findings[0]
	if finding.RuleID != "STORE-CONN-001" || finding.Symbol != "database/sql.Open" || finding.Path != "store/open.go" || finding.Line != 12 ||
		finding.Owner != "codefly.dev/store:1.5.0" || finding.Docs != "docs/usage.md" || !strings.Contains(finding.Do, "scoped reader or writer") {
		t.Fatalf("got %+v", finding)
	}
	// Two rules, one with a go clause: the other applies as text only.
	if owner := report.Owners[0]; owner.Rules != 2 || owner.Detectable != 1 || owner.Through != "dependency backend/store" {
		t.Fatalf("got %+v", owner)
	}

	var rendered bytes.Buffer
	Render(&rendered, report)
	for _, want := range []string{
		"codefly.dev/store:1.5.0 (dependency backend/store): 2 rule(s), 1 checkable here",
		"store/open.go:12:9: error STORE-CONN-001 call: database/sql.Open",
		"why: A raw connection bypasses",
		"do:  Request a scoped reader or writer",
		"see: docs/usage.md in codefly.dev/store:1.5.0",
		"backend/api: 1 error(s), 0 warning(s)",
	} {
		if !strings.Contains(rendered.String(), want) {
			t.Errorf("the rendering lacks %q:\n%s", want, rendered.String())
		}
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rule_id":"STORE-CONN-001"`, `"owner":"codefly.dev/store:1.5.0"`, `"errors":1`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the JSON lacks %s: %s", want, encoded)
		}
	}
}

func TestACleanServiceSaysSoAndAnUninspectedOneDoesNot(t *testing.T) {
	sources := []Source{{Owner: "codefly.dev/store:1.5.0", Through: "its own agent", Document: advertised(t)}}
	clean := consumer(basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE)
	clean.Symbols = nil
	report, err := Check("backend/api", sources, clean)
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	Render(&rendered, report)
	if report.Errors != 0 || !strings.Contains(rendered.String(), "backend/api: no usage rule violated") {
		t.Fatalf("got %+v\n%s", report, rendered.String())
	}

	degraded := consumer(basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_DEGRADED)
	degraded.Symbols = nil
	degraded.Issues = []*basev0.SemanticIssue{{Path: "store/broken.go", Message: "syntax error"}}
	report, err = Check("backend/api", sources, degraded)
	if err != nil {
		t.Fatal(err)
	}
	rendered.Reset()
	Render(&rendered, report)
	if len(report.Incomplete) != 2 || strings.Contains(rendered.String(), "no usage rule violated") ||
		!strings.Contains(rendered.String(), "backend/api: 0 error(s), 0 warning(s) in what was read; not everything could be read") ||
		!strings.Contains(rendered.String(), "store/broken.go: syntax error") {
		t.Fatalf("an index that is not complete is not a clean bill: %+v\n%s", report, rendered.String())
	}
}

func TestNoContractIsSaidPlainly(t *testing.T) {
	report, err := Check("backend/api", nil, consumer(basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE))
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	Render(&rendered, report)
	if !strings.Contains(rendered.String(), "no agent it consumes publishes a usage contract") {
		t.Fatalf("got %s", rendered.String())
	}
	if encoded, _ := json.Marshal(report); !strings.Contains(string(encoded), `"owners":[]`) || !strings.Contains(string(encoded), `"findings":[]`) {
		t.Fatalf("empty lists are lists, not null: %s", encoded)
	}
}

// A contract its reader refuses fails the check by owner: an unreadable rule
// set is not an empty one.
func TestAnUnreadableContractFailsTheCheckNamingItsOwner(t *testing.T) {
	sources := []Source{{Owner: "codefly.dev/store:1.5.0", Document: "schema: codefly/usage/v9\n"}}
	_, err := Check("backend/api", sources, consumer(basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE))
	if err == nil || !strings.Contains(err.Error(), "usage contract of codefly.dev/store:1.5.0") {
		t.Fatalf("got %v", err)
	}
}

// The command's status, case by case. The report is written in every one of
// them, a partial report included.
func TestTheCommandFailsOnAViolationAndOnAnIncompleteCheck(t *testing.T) {
	contract := []Source{{Owner: "codefly.dev/store:1.5.0", Through: "its own agent", Document: advertised(t)}}
	complete := basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE
	degraded := basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_DEGRADED
	clean := func(state basev0.SemanticIndexState) *basev0.SemanticIndex {
		index := consumer(state)
		index.Symbols = nil
		return index
	}
	// An analyzer that does not project calls outside functions: the index
	// is complete as far as it goes, and initializers went unread.
	older := clean(complete)
	older.FileCallsProjected = false
	// A service in a language no clause of the contract names.
	otherLanguage := clean(degraded)
	otherLanguage.Languages = []string{"rust"}
	otherLanguage.Files = []*basev0.SemanticFile{{Path: "src/lib.rs", Language: "rust"}}
	unknownLanguage := &basev0.SemanticIndex{State: degraded}
	unreadFile := &basev0.SemanticIndex{
		State: degraded, Languages: otherLanguage.Languages, Files: otherLanguage.Files,
		Issues: []*basev0.SemanticIssue{{Path: "unread.go", Message: "file too large"}},
	}

	for name, c := range map[string]struct {
		sources []Source
		index   *basev0.SemanticIndex
		fails   string // "" passes
		shows   string
	}{
		"clean and complete passes":                       {contract, clean(complete), "", "no usage rule violated"},
		"an error fails":                                  {contract, consumer(complete), "1 usage rule violation(s)", "error STORE-CONN-001"},
		"a file that could not be inspected fails":        {contract, clean(degraded), "could not be checked completely", "incomplete: the semantic index is"},
		"an error in a partial index is reported as one":  {contract, consumer(degraded), "1 usage rule violation(s)", "error STORE-CONN-001"},
		"initializers the analyzer did not project fail":  {contract, older, "could not be checked completely", "incomplete: calls outside functions and methods"},
		"incomplete without language information fails":   {contract, unknownLanguage, "could not be checked completely", "incomplete: the semantic index is"},
		"a missing file may be in a different language":   {contract, unreadFile, "could not be checked completely", "unread.go: file too large"},
		"incomplete with nothing detectable there passes": {contract, otherLanguage, "", "0 checkable here"},
		"incomplete with no contract at all passes":       {nil, clean(degraded), "", "no agent it consumes publishes a usage contract"},
	} {
		report, err := Check("backend/api", c.sources, c.index)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, machineReadable := range []bool{false, true} {
			var out bytes.Buffer
			status := finish(&out, report, machineReadable)
			if out.Len() == 0 {
				t.Errorf("%s: nothing was written before the status was decided", name)
			}
			switch {
			case c.fails == "" && status != nil:
				t.Errorf("%s: failed with %q, want a pass", name, status)
			case c.fails != "" && status == nil:
				t.Errorf("%s: passed, want a failure saying %q:\n%s", name, c.fails, out.String())
			case c.fails != "" && !machineReadable && !strings.Contains(status.Error(), c.fails):
				t.Errorf("%s: failed with %q, want %q", name, status, c.fails)
			case c.fails != "" && machineReadable && status.Error() != "":
				t.Errorf("%s: with --json the report is the message, got a second one: %q", name, status)
			}
			if !machineReadable && !strings.Contains(out.String(), c.shows) {
				t.Errorf("%s: the output lacks %q:\n%s", name, c.shows, out.String())
			}
			if machineReadable {
				var decoded Report
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded.Service != "backend/api" {
					t.Errorf("%s: stdout is not the report alone: %v\n%s", name, err, out.String())
				}
			}
		}
	}
}
