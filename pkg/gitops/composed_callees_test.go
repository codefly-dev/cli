package gitops

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	modulecontract "github.com/codefly-dev/core/contracts/module"
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

// What a call may carry beyond what it requires (a first write creating the
// collection it writes to) is part of what a binding naming the call may
// carry, answered beside what it requires.
func TestACallCarriesWhatItsCalleeDeclaresBesideWhatItRequires(t *testing.T) {
	callees := newComposedCallees(func(string) ([]*compositionv1.MethodPolicyV1, bool, error) {
		return []*compositionv1.MethodPolicyV1{{Method: "/documents.v1.DocumentsService/CreateDocument",
			Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "documents", Actions: []string{"write"}}},
			Carries:  []*authorityv1.WorkScopeV1{{ResourceKind: "documents.collection", Actions: []string{"create"}}}}}, true, nil
	})
	got, err := callees.Requires("documents", "/documents.v1.DocumentsService/CreateDocument")
	if err != nil || strings.Join(got, ",") != "documents:write,documents.collection:create" {
		t.Fatalf("CreateDocument: %v %v", got, err)
	}
}

// A composed module's bundle is read from that module's own contract, its
// includes expanded there; a module the workspace does not compose is an
// error naming why.
func TestComposedCalleesAnswerABundleFromThePublishersContract(t *testing.T) {
	callees := newComposedCallees(func(string) ([]*compositionv1.MethodPolicyV1, bool, error) { return nil, false, nil })
	if _, err := callees.Bundle("agent", "talk"); err == nil {
		t.Fatal("a bundle was answered with no contracts to read")
	}
	callees.contract = func(module string) (*modulecontract.Contract, error) {
		if module != "agent" {
			return nil, fmt.Errorf("%s is not composed", module)
		}
		return modulecontract.Parse([]byte(`schema: codefly/module-contract/v1
principal: agent
namespaces: []
queues: []
scope_ceilings: [{resource_kind: agent.turns, actions: [read, run]}]
bindings: []
destinations: []
bundles:
  - {name: talk, scopes: [{resource_kind: agent.turns, actions: [run]}]}
  - {name: review, includes: [talk], scopes: [{resource_kind: agent.turns, actions: [read]}]}
`))
	}
	got, err := callees.Bundle("agent", "review")
	if err != nil || len(got) != 1 || strings.Join(got[0].Actions, ",") != "read,run" {
		t.Fatalf("review: %+v, %v", got, err)
	}
	if _, err := callees.Bundle("notes", "talk"); err == nil || !strings.Contains(err.Error(), "notes is not composed") {
		t.Fatalf("an uncomposed module: %v", err)
	}
	var _ modulecontract.Bundles = callees
}

// A bundle may include another module's bundle, reached through the module
// including it. Such an include is read from the contract that other module
// publishes, so a bundle is answered over every composed module's contract
// and never from the including module's alone.
func TestComposedCalleesFollowABundleIntoAnotherModulesContract(t *testing.T) {
	published := map[string]string{
		"agent": `
  - name: talk
    includes: [{module: model, bundle: infer, through: agent}]
    scopes: [{resource_kind: agent.turns, actions: [run]}]
`,
		"model": `
  - {name: infer, scopes: [{resource_kind: model.profiles, actions: [invoke]}]}
`,
	}
	callees := newComposedCallees(func(string) ([]*compositionv1.MethodPolicyV1, bool, error) { return nil, false, nil })
	callees.contract = func(module string) (*modulecontract.Contract, error) {
		bundles, composed := published[module]
		if !composed {
			return nil, fmt.Errorf("%s is not composed", module)
		}
		return modulecontract.Parse([]byte("schema: codefly/module-contract/v1\nprincipal: " + module +
			"\nnamespaces: []\nqueues: []\nscope_ceilings: []\nbindings: []\ndestinations: []\nbundles:" + bundles))
	}
	got, err := callees.Bundle("agent", "talk")
	if err != nil {
		t.Fatalf("a bundle including another module's: %v", err)
	}
	want := []modulecontract.CeilingScope{
		{ResourceKind: "agent.turns", Actions: []string{"run"}},
		{ResourceKind: "model.profiles", Actions: []string{"invoke"}, Through: "agent"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("talk expands to %+v, want %+v", got, want)
	}
	// The module the include names is not composed: refused naming the chain.
	delete(published, "model")
	if _, err := callees.Bundle("agent", "talk"); err == nil || !strings.Contains(err.Error(), "model/infer") || !strings.Contains(err.Error(), "model is not composed") {
		t.Fatalf("an include of an uncomposed module's bundle: %v", err)
	}
}
