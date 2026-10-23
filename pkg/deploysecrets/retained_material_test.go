package deploysecrets

import (
	"context"
	"errors"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

type failOneWrite struct {
	*fakeStore
	key string
}

func (store *failOneWrite) Write(ctx context.Context, key string, document map[string]string, create bool) error {
	if key == store.key {
		return errors.New("controlled write failure")
	}
	return store.fakeStore.Write(ctx, key, document, create)
}

func TestPartialApplyReportsCompletedKeysAndRetryKeepsMaterial(t *testing.T) {
	store := &failOneWrite{fakeStore: newFakeStore(seeded()), key: "tasks-store"}
	inputs := &Inputs{Rendered: fixture(), Store: store, ReadPayloads: true, Generators: generators(), Services: []string{"tasks/store"}}
	plan, err := Build(context.Background(), inputs)
	require.NoError(t, err)
	written, err := plan.Apply(context.Background(), store, true)
	require.ErrorContains(t, err, "controlled write failure")
	require.Equal(t, []string{"notes-backend"}, written)
	retained := store.documents["notes-backend"][internalToken]
	store.key = ""
	plan, err = Build(context.Background(), inputs)
	require.NoError(t, err)
	written, err = plan.Apply(context.Background(), store, true)
	require.NoError(t, err)
	require.Equal(t, []string{"tasks-store"}, written)
	require.Equal(t, retained, store.documents["notes-backend"][internalToken])
}

func TestPlannerRefusesExistingKeyWithoutReadableVersion(t *testing.T) {
	for _, read := range []bool{false, true} {
		store := newFakeStore(seeded())
		store.empty["host-accounts"] = true
		_, err := Build(context.Background(), &Inputs{Rendered: fixture(), Store: store, ReadPayloads: read, Generators: generators()})
		require.ErrorContains(t, err, "without a readable version")
		require.Empty(t, store.writes)
	}
}

func TestPlannerRefusesChangesBeforeApply(t *testing.T) {
	for _, existing := range []bool{false, true} {
		store := newFakeStore(seeded())
		if !existing {
			delete(store.documents, "wiki-backend")
		}
		plan, err := Build(context.Background(), &Inputs{Rendered: fixture(), Store: store, ReadPayloads: true, Generators: generators()})
		require.NoError(t, err)
		store.documents["wiki-backend"] = map[string]string{internalToken: "changed-after-confirmation"}
		written, err := plan.Apply(context.Background(), store, true)
		require.ErrorContains(t, err, "changed since planning")
		require.Empty(t, written)
		require.Empty(t, store.writes)
	}
}

func TestSigningKeyRequiresExplicitMintAndNeverRemintsExistingDocument(t *testing.T) {
	generator := []environments.EnvironmentSecretGenerator{{Scope: environments.SecretGeneratorScopeService,
		Configuration: "postgres", Services: []string{"tasks/store"}, Keys: []string{"POSTGRES_PASSWORD"},
		Format: environments.SecretGeneratorFormatEd25519PKCS8}}
	store := newFakeStore(map[string]map[string]string{})
	inputs := &Inputs{Rendered: fixture(), Store: store, ReadPayloads: true, Generators: generator, Services: []string{"tasks/store"}}
	plan, err := Build(context.Background(), inputs)
	require.NoError(t, err)
	require.Equal(t, ActionRequire, propertyPlan(t, plan, "tasks-store", storePassword).Action)
	store.documents["tasks-store"] = map[string]string{"unrelated": "retained"}
	inputs.MintKeys = []string{storePassword}
	_, err = Build(context.Background(), inputs)
	require.ErrorContains(t, err, "never re-mint")
	require.Empty(t, store.writes)
}
