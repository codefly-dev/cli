package fixture

import (
	"encoding/json"
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
