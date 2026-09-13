// Package agentkinds owns the mapping between the short agent kind a user
// types ("runnable") and the kind core registers ("codefly:runnable").
//
// The `codefly:` namespace is core's spelling, not core's API: core exposes no
// short-form accessor, so every caller that wants one has to strip or add the
// prefix itself. Doing that in each caller means the convention is encoded in
// as many places as there are callers, and they drift independently. This
// package is the single place that knows it.
package agentkinds

import (
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// namespacePrefix is the namespace core registers its own agent kinds under.
const namespacePrefix = "codefly:"

var (
	kindByShort     map[string]resources.AgentKind
	ambiguousShorts map[string][]resources.AgentKind
	vocabulary      []string
)

func init() {
	kindByShort, ambiguousShorts, vocabulary = build(resources.AgentKindRegistry())
}

// build derives the short-form table from a registry. It is a pure function of
// its argument so the collision path below can be exercised with a synthetic
// registry: core registers no colliding pair today, and a test that can only
// observe today's registry would not catch the day one appears.
//
// TrimPrefix strips only "codefly:", so two kinds collide in exactly one
// shape: a namespaced kind and a bare kind equal to its short form
// ("codefly:service" and "service"). Those are distinct Resource values, so
// core's initializeAgentKindRegistry duplicate check passes them and
// ValidateAgentKindRegistration requires only that Resource be non-empty.
// Letting the later one win would silently resolve `--kind=service` to the
// wrong agent kind, so a collided short form maps to nothing and callers must
// spell the kind in full.
func build(registry []resources.AgentKindRegistration) (map[string]resources.AgentKind, map[string][]resources.AgentKind, []string) {
	claimants := make(map[string][]resources.AgentKind, len(registry))
	for _, registration := range registry {
		short := shorten(registration.Resource)
		claimants[short] = append(claimants[short], registration.Resource)
	}
	byShort := make(map[string]resources.AgentKind, len(registry))
	collisions := make(map[string][]resources.AgentKind)
	for short, kinds := range claimants {
		if len(kinds) == 1 {
			byShort[short] = kinds[0]
			continue
		}
		collisions[short] = kinds
	}
	// Registry order, so the published vocabulary is stable across processes.
	words := make([]string, 0, len(registry))
	for _, registration := range registry {
		short := shorten(registration.Resource)
		if _, collided := collisions[short]; collided {
			words = append(words, string(registration.Resource))
			continue
		}
		words = append(words, short)
	}
	return byShort, collisions, words
}

func shorten(kind resources.AgentKind) string {
	return strings.TrimPrefix(string(kind), namespacePrefix)
}

// Vocabulary is the ordered set of kind values a user may type, one per
// registered kind. A kind whose short form collides with another's is listed
// in full, because its short form is not accepted.
func Vocabulary() []string {
	return append([]string(nil), vocabulary...)
}

// Resolve turns what a user typed into the registered agent kind.
//
// It accepts the short form ("runnable"), the registered form
// ("codefly:runnable") and a registered alias ("codefly:service:builder"), and
// always returns the CANONICAL kind for the registration: an alias resolves to
// the kind it aliases, so a caller never carries an alias forward into an
// Agent it stores or reports. An empty input is the default kind, service.
func Resolve(input string) (resources.AgentKind, error) {
	if input == "" {
		return resources.ServiceAgent, nil
	}
	if strings.Contains(input, ":") {
		registration, err := resources.AgentKindRegistrationFor(resources.AgentKind(input))
		if err != nil {
			return "", fmt.Errorf("unknown agent kind %q: %w", input, err)
		}
		return registration.Resource, nil
	}
	if kind, ok := kindByShort[input]; ok {
		return kind, nil
	}
	if collided, ok := ambiguousShorts[input]; ok {
		return "", fmt.Errorf("agent kind %q is ambiguous: %s; name one in full", input, joinKinds(collided))
	}
	return "", fmt.Errorf("unknown agent kind %q: expected one of %s", input, strings.Join(Vocabulary(), ", "))
}

func joinKinds(kinds []resources.AgentKind) string {
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
