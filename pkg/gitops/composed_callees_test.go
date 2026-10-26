package gitops

import (
	"errors"
	"strings"
	"testing"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// The store's own declarations, as its published contracts carry them.
func storePolicies() []*compositionv1.MethodPolicyV1 {
	read := []*authorityv1.WorkScopeV1{{ResourceKind: "documents", Actions: []string{"read"}}, {ResourceKind: "documents.collection", Actions: []string{"read"}}}
	return []*compositionv1.MethodPolicyV1{
		{Method: "/documents.v1.DocumentsService/ResolveEvidence", Requires: read},
		{Method: "/documents.v1.DocumentsService/Version", AnyCheckedCaller: true},
		{Method: "/documents.v1.DocumentsService/Unmarked"},
	}
}

// What a call requires is the callee's own method policy: every scope it names,
// nothing for a call open to any checked caller; an unserved call, an unmarked
// one and an uncomposed callee are each refused by name. A callee is read once.
func TestACallRequiresWhatTheComposedCalleeDeclares(t *testing.T) {
	reads := 0
	callees := newComposedCallees(func(module string) ([]*compositionv1.MethodPolicyV1, bool, error) {
		reads++
		switch module {
		case "documents":
			return storePolicies(), true, nil
		case "bare":
			return nil, false, nil
		}
		return nil, false, errors.New("it is not a module this workspace composes")
	})
	got, err := callees.Requires("documents", "/documents.v1.DocumentsService/ResolveEvidence")
	if err != nil || strings.Join(got, ",") != "documents:read,documents.collection:read" {
		t.Fatalf("ResolveEvidence: %v %v", got, err)
	}
	if got, err := callees.Requires("documents", "/documents.v1.DocumentsService/Version"); err != nil || len(got) != 0 {
		t.Fatalf("an open call requires nothing: %v %v", got, err)
	}
	for call, want := range map[string]string{
		"/documents.v1.DocumentsService/Missing":  "does not serve it",
		"/documents.v1.DocumentsService/Unmarked": "declares nothing for it",
	} {
		if _, err := callees.Requires("documents", call); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", call, err)
		}
	}
	if reads != 1 {
		t.Fatalf("the callee was read %d times; once per resolution", reads)
	}
	if _, err := callees.Requires("bare", "/x.v1.S/M"); err == nil || !strings.Contains(err.Error(), "publishes no contracts") {
		t.Fatalf("a callee with no contracts: %v", err)
	}
	if _, err := callees.Requires("elsewhere", "/x.v1.S/M"); err == nil || !strings.Contains(err.Error(), "not a module this workspace composes") {
		t.Fatalf("an uncomposed callee: %v", err)
	}
}
