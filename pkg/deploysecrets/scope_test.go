package deploysecrets

import (
	"context"
	"maps"
	"slices"
	"sort"
	"testing"
)

func plannedKeys(plan *Plan) []string {
	keys := make([]string, 0, len(plan.Secrets))
	for _, secret := range plan.Secrets {
		keys = append(keys, secret.RemoteKey)
	}
	return keys
}

// A scoped plan covers only the keys the named module's own services read:
// nothing else is planned or written, although every existing key is still read
// so a scoped key keeps agreeing with what the store holds.
func TestScopedPlanCoversOnlyTheModulesKeys(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	store := newFakeStore(seeded())
	before := map[string]map[string]string{}
	for key, document := range store.documents {
		before[key] = maps.Clone(document)
	}

	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true, Modules: []string{"tasks"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := plannedKeys(plan); !slices.Equal(got, []string{"tasks-store", "tasks-worker"}) {
		t.Errorf("planned %v, want only the tasks keys", got)
	}
	reads := slices.Clone(store.reads)
	sort.Strings(reads)
	if !slices.Equal(reads, []string{"host-accounts", "wiki-backend"}) {
		t.Errorf("read %v, want every existing key read even outside the scope", reads)
	}
	if _, err := plan.Apply(ctx, store, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.writes, []string{"tasks-store"}) {
		t.Errorf("wrote %v, want only tasks-store", store.writes)
	}
	for key, document := range before {
		if !maps.Equal(store.documents[key], document) {
			t.Errorf("%s outside the scope changed", key)
		}
	}
}

// Scoping to a solution whose shared value another key already holds propagates
// from that key without planning or rewriting it: the holder is read, never
// written.
func TestScopedPlanPropagatesFromAnUnscopedHolder(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	store := newFakeStore(seeded())
	storedWiki := maps.Clone(store.documents["wiki-backend"])
	storedHost := maps.Clone(store.documents["host-accounts"])

	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true, Modules: []string{"notes"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := plannedKeys(plan); !slices.Equal(got, []string{"notes-backend"}) {
		t.Fatalf("planned %v, want notes-backend alone", got)
	}
	if got := propertyPlan(t, plan, "notes-backend", internalToken); got.Action != ActionPropagate {
		t.Errorf("shared token = %s (%s), want propagate from the keys outside the scope", got.Action, got.Source)
	}
	if _, err := plan.Apply(ctx, store, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Equal(store.writes, []string{"notes-backend"}) {
		t.Errorf("wrote %v, want notes-backend alone", store.writes)
	}
	if store.documents["notes-backend"][internalToken] != "shared-internal-token" {
		t.Error("the scoped key did not receive the value the store already holds")
	}
	if !maps.Equal(store.documents["wiki-backend"], storedWiki) || !maps.Equal(store.documents["host-accounts"], storedHost) {
		t.Error("a key outside the scope changed")
	}
}
