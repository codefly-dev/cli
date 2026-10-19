package install

import (
	"strings"
	"testing"
)

// The composition under test is the shape that motivated this: a module whose
// services talk to each other, and which reaches a host it does not contain.
func composition() []Service {
	return []Service{
		{
			Module: "record-store", Name: "records",
			Groups:       []string{"work-context", "platform", "internal-auth"},
			Dependencies: []string{"record-store/store/tcp", "saas/accounts/authority", "saas/auth-gateway/rest"},
		},
		{Module: "record-store", Name: "store"},
		{
			Module: "record-store", Name: "playground",
			Groups:       []string{"work-context"},
			Dependencies: []string{"record-store/records/connect"},
		},
	}
}

// What has to be granted is what leaves the composition. A dependency inside it
// is composed by the operator in the same act, so listing it as something to
// approve buries the two that genuinely cross a boundary.
func TestOnlyWhatLeavesTheCompositionIsSomethingToGrant(t *testing.T) {
	needs := Derive(composition())
	byService := map[string][]string{}
	for _, need := range needs {
		byService[need.Service.Unique()] = need.Reaches
	}
	records := byService["record-store/records"]
	if len(records) != 2 {
		t.Fatalf("records reaches %v; the two saas endpoints cross out of this composition and the store does not", records)
	}
	for _, reach := range records {
		if strings.HasPrefix(reach, "record-store/") {
			t.Errorf("%q is inside this composition and is composed, not granted", reach)
		}
	}
	if got := byService["record-store/playground"]; len(got) != 0 {
		t.Errorf("the playground only calls records, which this composition contains; got %v", got)
	}
}

// Deriving is not arranging. The text an approver reads has to say so, because
// a list of requirements reads like a list of things already handled.
func TestTheProposalSaysItGrantsNothing(t *testing.T) {
	rendered := Render(Derive(composition()))
	for _, required := range []string{"Nothing below is granted", "trust-root operation and is not written here"} {
		if !strings.Contains(rendered, required) {
			t.Errorf("the proposal does not say %q:\n%s", required, rendered)
		}
	}
	if !strings.Contains(rendered, "saas/accounts/authority") {
		t.Error("the proposal does not name what the composition reaches outside itself")
	}
}

// The same declarations must always give the same text: an operator approving
// what a deployed environment printed has to be approving what a local run
// derived from the same manifests, not a differently ordered reading of it.
func TestTheProposalIsStableForTheSameDeclarations(t *testing.T) {
	first := Render(Derive(composition()))
	shuffled := composition()
	shuffled[0], shuffled[2] = shuffled[2], shuffled[0]
	if second := Render(Derive(shuffled)); first != second {
		t.Errorf("the proposal depends on declaration order:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestAServiceThatNeedsNothingSaysSo(t *testing.T) {
	rendered := Render(Derive([]Service{{Module: "record-store", Name: "store"}}))
	if !strings.Contains(rendered, "reads no configuration and calls nothing outside") {
		t.Errorf("a service with no needs is rendered as a blank entry:\n%s", rendered)
	}
}

func TestAnEmptyCompositionIsNotAnInstall(t *testing.T) {
	if rendered := Render(Derive(nil)); !strings.Contains(rendered, "nothing to install") {
		t.Errorf("an empty composition renders as something to approve:\n%s", rendered)
	}
}
