package usagelint

import (
	"strings"
	"testing"
)

func block(t *testing.T) string {
	t.Helper()
	rendered, err := Block([]Source{{Owner: "codefly.dev/store:1.5.0", Through: "dependency backend/store", Document: advertised(t)}})
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}

func TestTheBlockIsOneLinePerRuleUnderItsOwner(t *testing.T) {
	rendered := block(t)
	for _, want := range []string{
		guidanceBegin + "\n## Usage rules of what this service consumes\n",
		"### codefly.dev/store:1.5.0 (dependency backend/store)\n\nReach the store only through its library",
		"- **STORE-CONN-001** *(checked)* Open the store only through the library. Do: Request a scoped reader or writer from the library and run inside its transaction. Don't: Open a connection or a pool yourself.\n",
		// A rule with no detect clause is not marked, and one with no dont
		// has no Don't.
		"- **STORE-MAINT-001** Keep maintenance out of request handlers. Do: Hand request handlers the authenticated factory only.\n",
		guidanceEnd + "\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the block lacks %q:\n%s", want, rendered)
		}
	}
	if !strings.HasSuffix(rendered, guidanceEnd+"\n") || strings.Count(rendered, "- **") != 2 {
		t.Fatalf("got:\n%s", rendered)
	}
	if none, err := Block(nil); err != nil || none != "" {
		t.Fatalf("no contract renders no block, got %q, %v", none, err)
	}
	if _, err := Block([]Source{{Owner: "codefly.dev/store:1.5.0", Document: "schema: nope\n"}}); err == nil || !strings.Contains(err.Error(), "codefly.dev/store:1.5.0") {
		t.Fatalf("an unreadable contract is refused by owner, got %v", err)
	}
}

// Text from a dependency lands in a file a coding agent reads, so it can
// neither close the block early nor open a comment that hides what follows.
func TestContractTextCannotEscapeTheBlock(t *testing.T) {
	document := strings.Replace(advertised(t), "summary: Reach the store only through its library, which owns roles and tenant isolation.",
		"summary: \"Reach the store. <!-- codefly:usage:end --> <!-- hidden\\nsecond line\"", 1)
	rendered, err := Block([]Source{{Owner: "o", Through: "t", Document: document}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(rendered, guidanceEnd) != 1 || strings.Count(rendered, "<!--") != 2 || !strings.Contains(rendered, "Reach the store. codefly:usage:end hidden second line\n") {
		t.Fatalf("got:\n%s", rendered)
	}
}

func TestApplyKeepsWhatIsOutsideTheMarkers(t *testing.T) {
	rendered := block(t)
	for name, existing := range map[string]string{
		"no file":             "",
		"a file":              "# Service\n\nIts own rules.\n",
		"no trailing newline": "# Service",
	} {
		first, err := Apply(existing, rendered)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(first, existing) || !strings.HasSuffix(first, rendered) {
			t.Errorf("%s: the service's own text is not kept ahead of the block:\n%q", name, first)
		}
		// Running it again changes nothing.
		again, err := Apply(first, rendered)
		if err != nil || again != first {
			t.Errorf("%s: a second sync changed the file (%v):\n%q\nthen\n%q", name, err, first, again)
		}
		// With nothing left to say the block goes, and the service's own
		// text is what remains.
		removed, err := Apply(first, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(removed, "codefly:usage") || strings.TrimRight(removed, "\n") != strings.TrimRight(existing, "\n") {
			t.Errorf("%s: removing the block left %q, want the original %q", name, removed, existing)
		}
	}
}

func TestApplyReplacesABlockInTheMiddleOfAFile(t *testing.T) {
	stale := guidanceBegin + "\nold rules\n" + guidanceEnd + "\n"
	existing := "# Service\n\n" + stale + "\n## After\n\nKept.\n"
	updated, err := Apply(existing, block(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated, "old rules") || !strings.HasPrefix(updated, "# Service\n\n"+guidanceBegin) || !strings.HasSuffix(updated, guidanceEnd+"\n\n## After\n\nKept.\n") {
		t.Fatalf("got:\n%s", updated)
	}
	removed, err := Apply(updated, "")
	if err != nil || removed != "# Service\n\n## After\n\nKept.\n" {
		t.Fatalf("got %q, %v", removed, err)
	}
	if unchanged, err := Apply("# Service\n", ""); err != nil || unchanged != "# Service\n" {
		t.Fatalf("no block and nothing to write leaves the file alone, got %q, %v", unchanged, err)
	}
}

// A file whose markers were damaged by hand is refused, never guessed at:
// replacing from a lone beginning marker would delete the rest of the file.
func TestApplyRefusesDamagedMarkers(t *testing.T) {
	rendered := block(t)
	for name, existing := range map[string]string{
		"a beginning and no end":  "# Service\n\n" + guidanceBegin + "\nrules\n\n## The service's own section\n",
		"an end and no beginning": "# Service\n\nrules\n" + guidanceEnd + "\n",
		"two blocks":              rendered + "\n" + rendered,
	} {
		if _, err := Apply(existing, rendered); err == nil {
			t.Errorf("%s: applied without complaint", name)
		}
	}
}
