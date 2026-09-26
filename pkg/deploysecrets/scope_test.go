package deploysecrets

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
)

func plannedKeys(plan *Plan) []string {
	keys := make([]string, 0, len(plan.Secrets))
	for _, secret := range plan.Secrets {
		keys = append(keys, secret.RemoteKey)
	}
	return keys
}

// A module with no federation plans only the keys its own services read: no
// solution, no registrar, nothing else is planned or written, although every key
// is still read.
func TestScopedPlanCoversOnlyTheModulesKeys(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	store := newFakeStore(seeded())
	before := map[string]map[string]string{}
	for key, document := range store.documents {
		before[key] = maps.Clone(document)
	}

	plan, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true, Modules: []string{"tasks"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := plannedKeys(plan); !slices.Equal(got, []string{"tasks-store", "tasks-worker"}) {
		t.Errorf("planned %v, want only the tasks keys", got)
	}
	if len(plan.Credentials) != 0 {
		t.Errorf("a module with no federation plans credentials %v", plan.Credentials)
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

// Scoping to a solution also plans its federation counterpart — the registrar's
// digest properties encoding a credential the solution holds — and names it as
// such; the registrar's other properties are neither planned nor rewritten, and
// the digests of credentials already stored are preserved byte for byte.
func TestScopedPlanIncludesTheFederationCounterpartOnly(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	store := newFakeStore(seeded())
	storedRegistrar := maps.Clone(store.documents["host-accounts"])
	storedWiki := maps.Clone(store.documents["wiki-backend"])

	plan, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true, Modules: []string{"notes"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := plannedKeys(plan); !slices.Equal(got, []string{"host-accounts", "notes-backend"}) {
		t.Fatalf("planned %v, want notes-backend and its registrar counterpart", got)
	}
	for _, secret := range plan.Secrets {
		switch secret.RemoteKey {
		case "host-accounts":
			if !slices.Equal(secret.Counterpart, []string{"notes"}) {
				t.Errorf("host-accounts counterpart = %v, want [notes]", secret.Counterpart)
			}
			var properties []string
			for _, property := range secret.Properties {
				properties = append(properties, property.Property)
				if !strings.Contains(property.Source, "federation counterpart of notes") {
					t.Errorf("%s source %q does not say it is the counterpart", property.Property, property.Source)
				}
			}
			if !slices.Equal(properties, []string{registrarDigest, solutionDigest}) {
				t.Errorf("counterpart plans %v, want only the digests of notes' credentials", properties)
			}
		case "notes-backend":
			if len(secret.Counterpart) != 0 {
				t.Error("the scoped module's own key is reported as a counterpart")
			}
		}
	}
	if got := propertyPlan(t, plan, "host-accounts", registrarDigest); got.Action != ActionKeep {
		t.Errorf("registration digests = %s, want keep: the registration secret notes reuses is already admitted", got.Action)
	}
	if got := propertyPlan(t, plan, "host-accounts", solutionDigest); got.Action != ActionUpdate {
		t.Errorf("solution digests = %s, want update", got.Action)
	}

	if _, err := plan.Apply(ctx, store, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Equal(store.writes, []string{"notes-backend", "host-accounts"}) {
		t.Errorf("wrote %v, want the plaintext holder then the registrar", store.writes)
	}
	if !maps.Equal(store.documents["wiki-backend"], storedWiki) {
		t.Error("wiki-backend, outside the scope, changed")
	}
	host := store.documents["host-accounts"]
	for key, value := range storedRegistrar {
		if key == solutionDigest {
			continue
		}
		if host[key] != value {
			t.Errorf("registrar property %s outside the counterpart changed", key)
		}
	}
	if !strings.Contains(host[solutionDigest], "wiki:"+digest("wiki-solution")) {
		t.Error("the stored wiki digest was not preserved byte for byte")
	}
	if !strings.Contains(host[solutionDigest], "notes:"+digest(store.documents["notes-backend"][solutionSecret])) {
		t.Error("the registrar does not admit the secret written for notes")
	}
}

// Scoping to the registrar alone would mint a solution's credential and store
// only its digest — the solution holding the plaintext is outside the scope — so
// the plan is refused, naming the credential.
func TestScopedPlanRefusesADigestWithoutItsHolder(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	store := newFakeStore(seeded())

	_, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true, Modules: []string{"host"}})
	if err == nil || !strings.Contains(err.Error(), notesSolution.String()) {
		t.Fatalf("Build scoped to the registrar = %v, want a refusal naming %s", err, notesSolution)
	}
	if len(store.writes) != 0 {
		t.Fatalf("a refused plan wrote %v", store.writes)
	}
}
