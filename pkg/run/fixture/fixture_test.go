package fixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func grants(pairs ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = json.RawMessage(pairs[i+1])
	}
	return out
}

// A solution composed on a platform base adds its own principal to the base's
// envelope policy without restating the base's grants, and decides a principal
// it states — the precedence configuration groups already have.
func TestCompositionGrantsAreTheBasesPlusTheSolutionsWhichDecide(t *testing.T) {
	base := GrantSource{Workspace: "platform-core", Principals: grants("documents", `{"tenant":"t"}`, "robin", `{"tenant":"t","x":1}`)}
	solution := GrantSource{Workspace: "wiki", Principals: grants("wiki", `{"tenant":"t"}`, "robin", `{"tenant":"t","x":2}`)}
	merged, err := MergeGrants(solution, []GrantSource{base})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 3 {
		t.Fatalf("merged = %v, want documents, robin and wiki", merged)
	}
	if string(merged["robin"]) != `{"tenant":"t","x":2}` {
		t.Fatalf("robin = %s, want the solution's grant", merged["robin"])
	}
	if string(merged["documents"]) != `{"tenant":"t"}` || string(merged["wiki"]) != `{"tenant":"t"}` {
		t.Fatalf("merged = %v", merged)
	}
}

// Two composed workspaces that grant one principal differently are refused by
// name unless the consuming workspace decides it; the same document spelled
// differently is one grant.
func TestCompositionGrantsRefuseASiblingDisagreementTheSolutionDoesNotDecide(t *testing.T) {
	a := GrantSource{Workspace: "a", Principals: grants("model", `{"tenant":"t","k":[1,2]}`)}
	b := GrantSource{Workspace: "b", Principals: grants("model", `{ "k": [1, 2], "tenant": "t" }`)}
	if _, err := MergeGrants(GrantSource{Workspace: "wiki"}, []GrantSource{a, b}); err != nil {
		t.Fatalf("the same grant in another spelling is not a disagreement: %v", err)
	}
	c := GrantSource{Workspace: "c", Principals: grants("model", `{"tenant":"other"}`)}
	_, err := MergeGrants(GrantSource{Workspace: "wiki"}, []GrantSource{a, c})
	if err == nil {
		t.Fatal("a disagreement nobody decides must be refused")
	}
	for _, want := range []string{`"model"`, `"a"`, `"c"`, `"wiki"`} {
		if !contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %s", err, want)
		}
	}
	if _, err := MergeGrants(GrantSource{Workspace: "wiki", Principals: grants("model", `{"tenant":"t"}`)}, []GrantSource{a, c}); err != nil {
		t.Fatalf("the consuming workspace decides: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) <= len(s) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// A floor is an integer of at least 1, read beside the principals; a file
// stating none, or no file, is no floor. ReadGrants never reads it, so the
// native run is untouched by it.
func TestMinimumRevisionIsAPositiveInteger(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "configurations", "prod"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "configurations", "prod", "module-envelope.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if floor, err := ReadMinimumRevision(dir, "prod"); err != nil || floor != 0 {
		t.Fatalf("no file = %d, %v", floor, err)
	}
	write(`{"principals":{"records":{}},"minimum_revision":7}`)
	if floor, err := ReadMinimumRevision(dir, "prod"); err != nil || floor != 7 {
		t.Fatalf("floor = %d, %v", floor, err)
	}
	principals, err := ReadGrants(dir, "prod")
	if err != nil || len(principals) != 1 {
		t.Fatalf("the grants beside a floor = %v, %v", principals, err)
	}
	for _, refused := range []string{`0`, `-1`, `1.5`, `"3"`, `null`, `1e3`} {
		write(`{"principals":{},"minimum_revision":` + refused + `}`)
		if _, err := ReadMinimumRevision(dir, "prod"); err == nil || !contains(err.Error(), "at least 1") {
			t.Fatalf("minimum_revision %s = %v, want refused", refused, err)
		}
	}
	write(`{"principals":{}}`)
	if floor, err := ReadMinimumRevision(dir, "prod"); err != nil || floor != 0 {
		t.Fatalf("no floor stated = %d, %v", floor, err)
	}
}

// The consuming workspace decides the floor; composed workspaces that state
// one must agree, or the disagreement is refused by name.
func TestCompositionMinimumRevisionIsDecidedNeverPicked(t *testing.T) {
	a, b, none := FloorSource{Workspace: "a", Floor: 4}, FloorSource{Workspace: "b", Floor: 9}, FloorSource{Workspace: "c"}
	if floor, err := MergeMinimumRevision(FloorSource{Workspace: "app"}, []FloorSource{a, none, {Workspace: "d", Floor: 4}}, "prod"); err != nil || floor != 4 {
		t.Fatalf("agreeing bases = %d, %v", floor, err)
	}
	_, err := MergeMinimumRevision(FloorSource{Workspace: "app"}, []FloorSource{a, b}, "prod")
	if err == nil {
		t.Fatal("a disagreement nobody decides must be refused")
	}
	for _, want := range []string{`"a"`, `"b"`, `"app"`, "4", "9"} {
		if !contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %s", err, want)
		}
	}
	if floor, err := MergeMinimumRevision(FloorSource{Workspace: "app", Floor: 2}, []FloorSource{a, b}, "prod"); err != nil || floor != 2 {
		t.Fatalf("the consuming workspace decides = %d, %v", floor, err)
	}
	if floor, err := MergeMinimumRevision(FloorSource{Workspace: "app"}, nil, "prod"); err != nil || floor != 0 {
		t.Fatalf("nobody states one = %d, %v", floor, err)
	}
}

// statesPrincipalsIn separates "this file states what the grants are" from
// "this file exists", because only the first may be delivered as a revocation.
// An authored {} states it; an absent key, an explicit null, and a file written
// for another key alone do not.
func TestStatesPrincipalsSeparatesAnAuthoredEmptyFromAnOmission(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "configurations", "prod"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "configurations", "prod", "module-envelope.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if stated, err := statesPrincipalsIn(dir, "prod"); err != nil || stated {
		t.Fatalf("no file = %v, %v", stated, err)
	}
	for _, body := range []string{`{}`, `{"minimum_revision":12}`, `{"principals":null}`, `{"principal":{}}`} {
		write(body)
		if stated, err := statesPrincipalsIn(dir, "prod"); err != nil || stated {
			t.Fatalf("%s = %v, %v, want not stated", body, stated, err)
		}
	}
	for _, body := range []string{`{"principals":{}}`, `{"principals":{"records":{}}}`, `{"principals":{},"minimum_revision":12}`} {
		write(body)
		if stated, err := statesPrincipalsIn(dir, "prod"); err != nil || !stated {
			t.Fatalf("%s = %v, %v, want stated", body, stated, err)
		}
	}
	write(`{"principals":`)
	if _, err := statesPrincipalsIn(dir, "prod"); err == nil {
		t.Fatal("a malformed file must be read as an error, never as no statement")
	}
}
