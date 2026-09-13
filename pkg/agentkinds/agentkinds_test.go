package agentkinds

import (
	"slices"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

func TestResolveAcceptsShortRegisteredAndAliasForms(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  resources.AgentKind
	}{
		{input: "", want: resources.ServiceAgent},
		{input: "service", want: resources.ServiceAgent},
		{input: "runnable", want: resources.RunnableAgent},
		{input: "codefly:runnable", want: resources.RunnableAgent},
		// An alias must canonicalize: carrying "codefly:service:builder"
		// forward puts an alias in the Agent the caller stores and reports.
		{input: string(resources.BuilderServiceAgent), want: resources.ServiceAgent},
	} {
		got, err := Resolve(tc.input)
		if err != nil {
			t.Fatalf("Resolve(%q) = %v", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestResolveRejectsUnknownKind(t *testing.T) {
	if _, err := Resolve("lambda"); err == nil {
		t.Fatal("unknown agent kind resolved")
	}
	if _, err := Resolve("codefly:lambda"); err == nil {
		t.Fatal("unknown registered agent kind resolved")
	}
}

func TestVocabularyIsRegistryOrderAndCoversEveryKind(t *testing.T) {
	registry := resources.AgentKindRegistry()
	words := Vocabulary()
	if len(words) != len(registry) {
		t.Fatalf("vocabulary has %d entries, registry has %d", len(words), len(registry))
	}
	for i, registration := range registry {
		if want := strings.TrimPrefix(string(registration.Resource), namespacePrefix); words[i] != want {
			t.Errorf("vocabulary[%d] = %q, want %q", i, words[i], want)
		}
		kind, err := Resolve(words[i])
		if err != nil {
			t.Fatalf("Resolve(%q) = %v", words[i], err)
		}
		if kind != registration.Resource {
			t.Errorf("Resolve(%q) = %q, want %q", words[i], kind, registration.Resource)
		}
	}
}

func TestVocabularyIsACopy(t *testing.T) {
	first := Vocabulary()
	first[0] = "mutated"
	if Vocabulary()[0] == "mutated" {
		t.Fatal("Vocabulary handed out its backing array")
	}
}

// TestCollidingShortFormsResolveToNothing is the case core cannot produce
// today and its registry validation cannot reject. TrimPrefix only strips
// "codefly:", so two kinds collide only in one shape: a namespaced kind and a
// BARE kind equal to its short form ("codefly:service" and "service"). Those
// are distinct Resource values, so core's duplicate check passes them, and
// ValidateAgentKindRegistration requires only that Resource be non-empty.
// Letting the later registration win would make `--kind=service` load the
// wrong agent kind with no error, so the short form must be refused and both
// kinds listed in full.
func TestCollidingShortFormsResolveToNothing(t *testing.T) {
	registry := []resources.AgentKindRegistration{
		{ProtoKind: basev0.Agent_SERVICE, Resource: resources.ServiceAgent},
		{ProtoKind: basev0.Agent_RUNNABLE, Resource: resources.RunnableAgent},
		{ProtoKind: basev0.Agent_PROVIDER, Resource: "service"},
	}
	byShort, collisions, words := build(registry)

	if _, ok := byShort["service"]; ok {
		t.Error(`colliding short form "service" still resolves to a single kind`)
	}
	if got := collisions["service"]; len(got) != 2 {
		t.Errorf(`collisions["service"] = %v, want both kinds`, got)
	}
	if !slices.Contains(words, "codefly:service") || !slices.Contains(words, "service") {
		t.Errorf("vocabulary %q does not list both colliding kinds in full", words)
	}
	// The kind that does not collide keeps its short form.
	if !slices.Contains(words, "runnable") {
		t.Errorf("vocabulary %q dropped the non-colliding short form", words)
	}
	if byShort["runnable"] != resources.RunnableAgent {
		t.Errorf(`byShort["runnable"] = %q, want %q`, byShort["runnable"], resources.RunnableAgent)
	}
}
