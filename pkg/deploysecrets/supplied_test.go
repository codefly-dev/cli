package deploysecrets

import (
	"context"
	"strings"
	"testing"
)

const externalValue = "operator-supplied-secret"

// The staging gap: an external client secret nothing can generate is handed in
// by the operator and written, while the rest of the plan is unchanged.
func TestASuppliedValueFillsARequiredProperty(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	store := newFakeStore(map[string]map[string]string{})
	supplied := map[string]string{clientSecret: externalValue}
	if err := ValidateSupplied(rendered, supplied, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store"}, Store: store, ReadPayloads: true, Supplied: supplied})
	if err != nil {
		t.Fatal(err)
	}
	got := propertyPlan(t, plan, "host-accounts", clientSecret)
	if got.Action != ActionSupply || strings.Contains(got.Source, externalValue) {
		t.Fatalf("plan = %+v, want supply without the value", got)
	}
	if _, err := plan.Apply(ctx, store, true); err != nil {
		t.Fatal(err)
	}
	if store.documents["host-accounts"][clientSecret] != externalValue {
		t.Error("the supplied value was not written")
	}
	// A rerun with the same value keeps it.
	plan, err = Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store"}, Store: store, ReadPayloads: true, Supplied: supplied})
	if err != nil {
		t.Fatal(err)
	}
	if got := propertyPlan(t, plan, "host-accounts", clientSecret); got.Action != ActionKeep {
		t.Errorf("rerun = %s, want keep", got.Action)
	}
}

func TestASuppliedValueNeverReplacesAStoredOne(t *testing.T) {
	_, err := Build(context.Background(), &Inputs{Rendered: fixture(), Generators: generators(),
		Services: []string{"tasks/store"}, Store: newFakeStore(seeded()), ReadPayloads: true,
		Supplied: map[string]string{clientSecret: externalValue}})
	if err == nil || !strings.Contains(err.Error(), "never replaces") || strings.Contains(err.Error(), externalValue) {
		t.Fatalf("Build = %v, want the stored value kept and the conflict refused", err)
	}
}

func TestASuppliedValueNeverStandsInForAGenerator(t *testing.T) {
	_, err := Build(context.Background(), &Inputs{Rendered: fixture(), Generators: generators(),
		Services: []string{"tasks/store"}, Store: newFakeStore(map[string]map[string]string{}), ReadPayloads: true,
		Supplied: map[string]string{internalToken: externalValue}})
	if err == nil || !strings.Contains(err.Error(), "service-secrets.generate") {
		t.Fatalf("Build = %v, want a supplied generated key refused", err)
	}
}

func TestSuppliedValuesMustBeReadAndNonEmptyUnlessDeclared(t *testing.T) {
	rendered := fixture()
	if err := ValidateSupplied(rendered, map[string]string{clientSecret + "_TYPO": "x"}, nil); err == nil {
		t.Error("a key no property reads was accepted")
	}
	if err := ValidateSupplied(rendered, map[string]string{clientSecret: ""}, nil); err == nil {
		t.Error("an undeclared empty value was accepted")
	}
	if err := ValidateSupplied(rendered, map[string]string{clientSecret: ""}, []string{clientSecret}); err != nil {
		t.Errorf("a declared empty value was refused: %v", err)
	}
}

// A declared-empty key is written empty, so the ExternalSecret that reads it
// resolves instead of failing on a missing property.
func TestADeclaredEmptySuppliedValueIsWritten(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore(map[string]map[string]string{})
	plan, err := Build(ctx, &Inputs{Rendered: fixture(), Generators: generators(),
		Services: []string{"tasks/store"}, Store: store, ReadPayloads: true,
		MayBeEmpty: []string{clientSecret}, Supplied: map[string]string{clientSecret: ""}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(ctx, store, true); err != nil {
		t.Fatal(err)
	}
	if value, present := store.documents["host-accounts"][clientSecret]; !present || value != "" {
		t.Errorf("declared-empty property = %q, %v; want written empty", value, present)
	}
}
