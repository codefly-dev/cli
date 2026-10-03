package deploysecrets

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

// fakeStore is an in-memory store that records every call, so a test can prove
// what was read and written.
type fakeStore struct {
	documents map[string]map[string]string
	// empty are keys that exist with no enabled version.
	empty map[string]bool
	// Build describes and reads the keys concurrently, so the recording is
	// guarded; the recorded order is not meaningful and tests compare sets.
	mutex  sync.Mutex
	reads  []string
	writes []string
}

func newFakeStore(documents map[string]map[string]string) *fakeStore {
	return &fakeStore{documents: documents, empty: map[string]bool{}}
}

func (store *fakeStore) Name() string { return "fake" }

func (store *fakeStore) Describe(_ context.Context, key string) (Description, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if store.empty[key] {
		return Description{Exists: true}, nil
	}
	_, exists := store.documents[key]
	return Description{Exists: exists, HasVersion: exists}, nil
}

func (store *fakeStore) Read(_ context.Context, key string) (map[string]string, error) {
	store.mutex.Lock()
	store.reads = append(store.reads, key)
	document, ok := store.documents[key]
	store.mutex.Unlock()
	if !ok {
		return nil, fmt.Errorf("no %s", key)
	}
	return maps.Clone(document), nil
}

func (store *fakeStore) Write(_ context.Context, key string, document map[string]string, create bool) error {
	_, exists := store.documents[key]
	if create == exists && !store.empty[key] {
		return fmt.Errorf("write %s with create=%v on a key that exists=%v", key, create, exists)
	}
	store.writes = append(store.writes, key)
	store.documents[key] = maps.Clone(document)
	delete(store.empty, key)
	return nil
}

const (
	internalToken   = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__CODEFLY_INTERNAL_TOKEN"
	clientSecret    = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__IDENTITY__IDENTITY_CLIENT_SECRET"
	storePassword   = "CODEFLY__SERVICE_SECRET_CONFIGURATION__TASKS__STORE__POSTGRES__POSTGRES_PASSWORD"
	storeConnection = "CODEFLY__SERVICE_SECRET_CONFIGURATION__TASKS__STORE__POSTGRES__READ_WRITE_CONNECTION"
	migration       = "CODEFLY_POSTGRES_MIGRATION_CONNECTION"
)

// filed files each secret key under a property of its own name — the shape an
// environment that declares no remote-key mapping renders. `mapped` is the other
// shape, where the environment names the property itself.
func filed(keys ...string) []gitops.RenderedSecretProperty {
	properties := make([]gitops.RenderedSecretProperty, 0, len(keys))
	for _, key := range keys {
		properties = append(properties, gitops.RenderedSecretProperty{Property: key, Keys: []string{key}})
	}
	return properties
}

// mapped files a secret key under a store property of a different name, the way
// `service-secrets.services.<svc>.remote-keys` does.
func mapped(property string, keys ...string) gitops.RenderedSecretProperty {
	return gitops.RenderedSecretProperty{Property: property, Keys: keys}
}

// fixture is a composition in the shape the staging cell has: a host whose
// accounts service holds an external client secret and the shared internal
// token, two solutions reading that token, and a module with a database.
func fixture() gitops.RenderedEnvironment {
	store := environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}
	return gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: store, RemoteKey: "host-accounts", Services: []string{"host/accounts"}, Properties: filed(clientSecret, internalToken)},
		{Store: store, RemoteKey: "notes-backend", Services: []string{"notes/backend"}, Properties: filed(internalToken)},
		{Store: store, RemoteKey: "tasks-store", Services: []string{"tasks/store"}, Properties: filed(migration, storePassword)},
		{Store: store, RemoteKey: "tasks-worker", Services: []string{"tasks/worker"}, Properties: filed(storeConnection)},
		{Store: store, RemoteKey: "wiki-backend", Services: []string{"wiki/backend"}, Properties: filed(internalToken)},
	}}
}

// seeded is the store as a hand-seeded cell leaves it: the host and one
// solution wired, a second solution and a new module never seeded.
func seeded() map[string]map[string]string {
	return map[string]map[string]string{
		"host-accounts": {
			clientSecret:  "external-client-secret",
			internalToken: "shared-internal-token",
		},
		"wiki-backend": {
			internalToken: "shared-internal-token",
		},
	}
}

func propertyPlan(t *testing.T, plan *Plan, key, property string) PropertyPlan {
	t.Helper()
	for _, secret := range plan.Secrets {
		if secret.RemoteKey != key {
			continue
		}
		for _, candidate := range secret.Properties {
			if candidate.Property == property {
				return candidate
			}
		}
	}
	t.Fatalf("no plan for %s#%s", key, property)
	return PropertyPlan{}
}

func generators() []environments.EnvironmentSecretGenerator {
	return []environments.EnvironmentSecretGenerator{
		{Scope: environments.SecretGeneratorScopeService, Configuration: "postgres", Keys: []string{"POSTGRES_PASSWORD"}},
		{Scope: environments.SecretGeneratorScopeWorkspace, Configuration: "internal-auth", Keys: []string{"CODEFLY_INTERNAL_TOKEN"}},
	}
}

// The staging shape: a second solution and a new module join a hand-seeded
// cell. The solution is handed the shared token the others hold, the module's
// declared password is generated and its external connection named — while
// nothing already stored is rotated.
func TestPlanSeedsANewModuleConsistentlyWithTheSeededCell(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	store := newFakeStore(seeded())

	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, check := range []struct {
		key, property string
		action        Action
	}{
		{"host-accounts", clientSecret, ActionKeep},
		{"host-accounts", internalToken, ActionKeep},
		{"wiki-backend", internalToken, ActionKeep},
		{"notes-backend", internalToken, ActionPropagate},
		{"tasks-store", storePassword, ActionGenerate},
		{"tasks-store", migration, ActionRequire},
		{"tasks-worker", storeConnection, ActionRequire},
	} {
		if got := propertyPlan(t, plan, check.key, check.property); got.Action != check.action {
			t.Errorf("%s#%s = %s (%s), want %s", check.key, check.property, got.Action, got.Source, check.action)
		}
	}
	if _, err := plan.Apply(ctx, store, false); err == nil || !strings.Contains(err.Error(), migration) {
		t.Fatalf("Apply with required properties = %v, want a refusal naming them", err)
	}
	if len(store.writes) != 0 {
		t.Fatalf("a refused apply wrote %v", store.writes)
	}

	written, err := plan.Apply(ctx, store, true)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Writes follow the render's order, and only keys with something to write.
	if !slices.Equal(written, []string{"notes-backend", "tasks-store"}) {
		t.Errorf("written %v, want notes-backend then tasks-store", written)
	}
	if store.documents["notes-backend"][internalToken] != "shared-internal-token" {
		t.Error("the shared token was not propagated")
	}
	host := store.documents["host-accounts"]
	if host[clientSecret] != "external-client-secret" || host[internalToken] != "shared-internal-token" {
		t.Error("an already-stored value was rewritten")
	}
	if len(store.documents["tasks-store"][storePassword]) != 64 {
		t.Error("the declared generator did not produce a 32-byte hex value")
	}
	if _, present := store.documents["tasks-store"][migration]; present {
		t.Error("a required property was written with no source")
	}

	// Re-planning the applied store changes nothing but what still has no source.
	again, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true})
	if err != nil {
		t.Fatal(err)
	}
	if changes := again.Changes(); len(changes) != 0 {
		t.Errorf("a second plan would write %v", changes)
	}
}

// A plan is reported by name and source only: no value — stored or generated —
// appears anywhere in it.
func TestPlanNeverCarriesAValue(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	documents := seeded()
	store := newFakeStore(documents)
	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store"}, Store: store, ReadPayloads: true})
	if err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for _, document := range documents {
		for _, value := range document {
			secrets = append(secrets, value)
		}
	}
	for _, write := range plan.writes {
		for _, value := range write.document {
			secrets = append(secrets, value)
		}
	}
	reported := fmt.Sprintf("%+v %+v %v", plan.Secrets, plan.Changes(), plan.Required())
	for _, secret := range secrets {
		if strings.Contains(reported, secret) {
			t.Errorf("the reported plan carries a value")
		}
	}
}

// Metadata only: nothing is read, existing keys are unverified, and the plan
// cannot be applied — it would overwrite properties it never saw.
func TestMetadataOnlyPlanReadsNothingAndCannotApply(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	store := newFakeStore(seeded())
	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store"}, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.reads) != 0 {
		t.Fatalf("a metadata-only plan read %v", store.reads)
	}
	if got := propertyPlan(t, plan, "host-accounts", clientSecret); got.Action != ActionUnverified {
		t.Errorf("an unread key's property = %s, want unverified", got.Action)
	}
	// A missing key is known to hold nothing, so it is still planned exactly.
	if got := propertyPlan(t, plan, "tasks-store", storePassword); got.Action != ActionGenerate {
		t.Errorf("a missing key's generated property = %s, want generate", got.Action)
	}
	if got := propertyPlan(t, plan, "notes-backend", internalToken); got.Action != ActionUnverified {
		t.Errorf("a shared property whose holders were not read = %s, want unverified", got.Action)
	}
	if _, err := plan.Apply(ctx, store, true); err == nil {
		t.Fatal("a metadata-only plan applied")
	}
	if len(store.writes) != 0 {
		t.Fatalf("wrote %v", store.writes)
	}
}

// A configuration value is one value: two keys holding it differently cannot be
// propagated from.
func TestPlanRefusesToPropagateAConflictingValue(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	documents := seeded()
	documents["wiki-backend"][internalToken] = "a-different-token"
	_, err := Build(ctx, &Inputs{Rendered: rendered, Store: newFakeStore(documents), ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), internalToken) {
		t.Fatalf("Build = %v, want a refusal naming the key", err)
	}
}

// With nothing stored at all, a generated shared value is minted once and is
// one value everywhere it is read; a key that exists with no enabled version is
// written like a missing one.
func TestPlanOnAnEmptyStoreGeneratesOnceAndAgrees(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	store := newFakeStore(map[string]map[string]string{})
	store.empty["host-accounts"] = true
	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store"}, Store: store, ReadPayloads: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := propertyPlan(t, plan, "host-accounts", clientSecret); got.Action != ActionRequire {
		t.Errorf("an external secret = %s, want require", got.Action)
	}
	if _, err := plan.Apply(ctx, store, true); err != nil {
		t.Fatal(err)
	}
	wiki, notes, host := store.documents["wiki-backend"], store.documents["notes-backend"], store.documents["host-accounts"]
	if wiki[internalToken] == "" || wiki[internalToken] != notes[internalToken] || host[internalToken] != wiki[internalToken] {
		t.Error("a generated shared value differs between the keys reading it")
	}
	if !slices.Contains(store.writes, "host-accounts") {
		t.Error("a key with no enabled version was not written")
	}
}

func TestGeneratorCoveringAKeyTwiceIsRefused(t *testing.T) {
	ctx := context.Background()
	rendered := fixture()
	duplicate := append(generators(), environments.EnvironmentSecretGenerator{
		Scope: environments.SecretGeneratorScopeService, Configuration: "postgres", Services: []string{"tasks/store"}, Keys: []string{"POSTGRES_PASSWORD"}})
	_, err := Build(ctx, &Inputs{Rendered: rendered, Generators: duplicate,
		Services: []string{"tasks/store"}, Store: newFakeStore(seeded()), ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("Build = %v, want a refusal", err)
	}
}

// One configuration value is one value however many services read it. Two keys
// holding it with different values is a store that no longer agrees with itself,
// and reporting both as `keep` is the one reading an operator cannot act on —
// so it is refused whether or not a third key needs it propagated.
func TestPlanRefusesAConfigurationValueHeldWithTwoValues(t *testing.T) {
	rendered := fixture()
	documents := seeded()
	documents["notes-backend"] = map[string]string{internalToken: "a-different-internal-token"}
	_, err := Build(context.Background(), &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: newFakeStore(documents), ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), "cannot be two") {
		t.Fatalf("Build = %v, want a refusal naming the divergence", err)
	}
	if strings.Contains(err.Error(), "a-different-internal-token") || strings.Contains(err.Error(), "shared-internal-token") {
		t.Errorf("the refusal quotes a stored value: %v", err)
	}
}

// The keys are gathered together, so a failure must still name the key that
// actually failed. A key skipped because another one failed carries no error of
// its own, and returning its cancellation instead would name nothing an operator
// can act on.
func TestBuildReportsTheKeyThatFailedNotTheOnesItCancelled(t *testing.T) {
	rendered := fixture()
	store := &failingStore{fakeStore: newFakeStore(seeded()), failRead: "wiki-backend"}
	_, err := Build(context.Background(), &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), "wiki-backend") {
		t.Fatalf("Build = %v, want the failing key named", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Errorf("the cancellation masked the failure: %v", err)
	}
}

// failingStore refuses to read one key, and is slow on the others so that they
// are still in flight when it does.
type failingStore struct {
	*fakeStore
	failRead string
}

func (store *failingStore) Read(ctx context.Context, key string) (map[string]string, error) {
	if key == store.failRead {
		return nil, fmt.Errorf("permission denied on %s", key)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(50 * time.Millisecond):
	}
	return store.fakeStore.Read(ctx, key)
}

// mappedFixture is the same composition as fixture(), rendered by an
// environment that files every secret key under a property of the store's own
// naming — `service-secrets.services.<svc>.remote-keys`, which is how a cell
// whose vault documents predate codefly is wired. No property equals its key,
// and the shared token is filed under three different names.
func mappedFixture() gitops.RenderedEnvironment {
	store := environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}
	return gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: store, RemoteKey: "host-accounts", Services: []string{"host/accounts"}, Properties: []gitops.RenderedSecretProperty{
			mapped("client_secret", clientSecret),
			mapped("shared_token", internalToken),
		}},
		{Store: store, RemoteKey: "notes-backend", Services: []string{"notes/backend"}, Properties: []gitops.RenderedSecretProperty{
			mapped("token", internalToken),
		}},
		{Store: store, RemoteKey: "tasks-store", Services: []string{"tasks/store"}, Properties: []gitops.RenderedSecretProperty{
			mapped("migration_connection", migration),
			mapped("store_password", storePassword),
		}},
		{Store: store, RemoteKey: "tasks-worker", Services: []string{"tasks/worker"}, Properties: []gitops.RenderedSecretProperty{
			mapped("store_read_write_connection", storeConnection),
		}},
		{Store: store, RemoteKey: "wiki-backend", Services: []string{"wiki/backend"}, Properties: []gitops.RenderedSecretProperty{
			mapped("internal_token", internalToken),
		}},
	}}
}

// mappedSeeded is seeded() written under the mapped property names.
func mappedSeeded() map[string]map[string]string {
	return map[string]map[string]string{
		"host-accounts": {
			"client_secret": "external-client-secret",
			"shared_token":  "shared-internal-token",
		},
		"wiki-backend": {
			"internal_token": "shared-internal-token",
		},
	}
}

// The verb's answer must not depend on what the environment named the store
// properties. Resolving by the property instead of by the secret key read out of
// it matched no shared configuration value and no declared generator here, so
// every property in this shape reported as `require` — a hand-typed secret for
// each of the classes the system derives itself.
func TestPlanResolvesByTheSecretKeyNotByTheStorePropertyName(t *testing.T) {
	ctx := context.Background()
	rendered := mappedFixture()
	store := newFakeStore(mappedSeeded())

	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, check := range []struct {
		key, property string
		action        Action
	}{
		// One configuration value filed under three different property names is
		// still one value, propagated to the remote key that lacks it.
		{"notes-backend", "token", ActionPropagate},
		{"host-accounts", "shared_token", ActionKeep},
		{"wiki-backend", "internal_token", ActionKeep},
		// A declared generator is declared over the key, not the property.
		{"tasks-store", "store_password", ActionGenerate},
		// Supplied from outside either way, and named.
		{"host-accounts", "client_secret", ActionKeep},
		{"tasks-store", "migration_connection", ActionRequire},
		{"tasks-worker", "store_read_write_connection", ActionRequire},
	} {
		if got := propertyPlan(t, plan, check.key, check.property); got.Action != check.action {
			t.Errorf("%s#%s = %s (%s), want %s", check.key, check.property, got.Action, got.Source, check.action)
		}
	}
	if _, err := plan.Apply(ctx, store, true); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The propagated value is the one the store already holds, not a new one.
	if got := store.documents["notes-backend"]["token"]; got != "shared-internal-token" {
		t.Errorf("propagated token = %q", got)
	}
	// What must be supplied names the key as well as the property: the property
	// alone says where the value is filed, never what it is.
	required := plan.Required()
	if !slices.Contains(required, "tasks-worker#store_read_write_connection ("+storeConnection+")") {
		t.Errorf("required = %v", required)
	}
}

// The plan reports the key beside the property for every entry, so an operator
// reading "require postgres_user" can tell which secret that is.
func TestPlanReportsTheKeysReadFromEachProperty(t *testing.T) {
	ctx := context.Background()
	rendered := mappedFixture()
	plan, err := Build(ctx, &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: newFakeStore(mappedSeeded()), ReadPayloads: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := propertyPlan(t, plan, "tasks-store", "store_password")
	if !reflect.DeepEqual(got.Keys, []string{storePassword}) {
		t.Errorf("keys = %v", got.Keys)
	}
}

// A property the render recorded no key for says where a value lives and never
// what it is. Every source this package resolves is named by the key, so such a
// property would fall to `require` whatever it actually is — the hand-typed
// outcome, arrived at silently. Refuse instead.
func TestPlanRefusesAPropertyWithNoSecretKey(t *testing.T) {
	rendered := fixture()
	rendered.Secrets[2].Properties[1].Keys = nil
	_, err := Build(context.Background(), &Inputs{Rendered: rendered, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: newFakeStore(seeded()), ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), "records no secret key") {
		t.Fatalf("err = %v", err)
	}
}

// Two keys filed under one property, covered by generators that disagree, have
// no answer: whichever won would be an accident of ordering.
func TestPlanRefusesOnePropertyGeneratedTwoWays(t *testing.T) {
	rendered := fixture()
	other := "CODEFLY__SERVICE_SECRET_CONFIGURATION__TASKS__STORE__POSTGRES__POSTGRES_USER"
	rendered.Secrets[2].Properties[1].Keys = []string{storePassword, other}
	declared := append(generators(), environments.EnvironmentSecretGenerator{
		Scope: environments.SecretGeneratorScopeService, Configuration: "postgres",
		Keys: []string{"POSTGRES_USER"}, Format: environments.SecretGeneratorFormatIdentifier})
	_, err := Build(context.Background(), &Inputs{Rendered: rendered, Generators: declared,
		Services: []string{"tasks/store", "tasks/worker"}, Store: newFakeStore(seeded()), ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), "declares differently") {
		t.Fatalf("err = %v", err)
	}
}

// An empty stored value is refused by default, because an empty credential is
// almost always a half-finished seed rather than a decision.
func TestBuildRefusesAPresentButEmptyProperty(t *testing.T) {
	rendered := fixture()
	store := newFakeStore(withEmpty(seeded(), "host-accounts", clientSecret))
	_, err := Build(context.Background(), &Inputs{Rendered: rendered,
		Generators: generators(), Services: []string{"tasks/store", "tasks/worker"},
		Store: store, ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("Build = %v, want the empty property refused", err)
	}
	if !strings.Contains(err.Error(), clientSecret) {
		t.Errorf("the error does not name the property: %v", err)
	}
	// The refusal must say how to declare the exception, or the operator's only
	// visible way out is to invent a value.
	if !strings.Contains(err.Error(), "may-be-empty") {
		t.Errorf("the error does not name the declaration: %v", err)
	}
}

// Some keys have no value to hold: a Sentry DSN where there is no Sentry
// project, a WebAuthn origin on a cell with no browser-facing origin.
func TestBuildAcceptsAnEmptyPropertyTheEnvironmentDeclares(t *testing.T) {
	rendered := fixture()
	store := newFakeStore(withEmpty(seeded(), "host-accounts", clientSecret))
	if _, err := Build(context.Background(), &Inputs{Rendered: rendered,
		Generators: generators(), Services: []string{"tasks/store", "tasks/worker"},
		Store: store, ReadPayloads: true, MayBeEmpty: []string{clientSecret}}); err != nil {
		t.Fatalf("Build = %v, want the declared key accepted", err)
	}
}

// Declaring one of a property's keys is not declaring the property: a property
// two services read under two keys is only safely empty when neither needs one.
func TestBuildStillRefusesWhenOnlySomeOfAPropertysKeysAreDeclared(t *testing.T) {
	rendered := fixture()
	two := gitops.RenderedSecretProperty{
		Property: "shared",
		Keys:     []string{"NEEDS_A_VALUE", "HOLDS_NOTHING"},
		Readers:  []gitops.RenderedSecretReader{{Service: "host/accounts", Key: "NEEDS_A_VALUE"}},
	}
	rendered.Secrets[0].Properties = append(rendered.Secrets[0].Properties, two)
	documents := seeded()
	documents["host-accounts"]["shared"] = ""
	store := newFakeStore(documents)
	_, err := Build(context.Background(), &Inputs{Rendered: rendered,
		Generators: generators(), Services: []string{"tasks/store", "tasks/worker"},
		Store: store, ReadPayloads: true, MayBeEmpty: []string{"HOLDS_NOTHING"}})
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("Build = %v, want the partly-declared property still refused", err)
	}
}

// A scoped run reads every remote key so a scoped key keeps agreeing with the
// store, but an out-of-scope key it will never write is not its business to
// refuse -- otherwise one unrelated service's optional key makes --module
// unusable, which is what happened on hosted-gcp-staging.
func TestBuildIgnoresAnEmptyPropertyOutsideTheModuleScope(t *testing.T) {
	rendered := fixture()
	store := newFakeStore(withEmpty(seeded(), "host-accounts", clientSecret))
	if _, err := Build(context.Background(), &Inputs{Rendered: rendered,
		Generators: generators(), Services: []string{"tasks/store", "tasks/worker"},
		Store: store, ReadPayloads: true, Modules: []string{"tasks"}}); err != nil {
		t.Fatalf("Build = %v, want an out-of-scope empty key ignored", err)
	}
}

// ...but an empty key the scoped run WOULD write is still refused, or scoping
// would become a way to write a key whose stored value is half-finished.
func TestBuildStillRefusesAnEmptyPropertyInsideTheModuleScope(t *testing.T) {
	rendered := fixture()
	documents := seeded()
	documents["tasks-store"] = map[string]string{migration: "", storePassword: "held"}
	store := newFakeStore(documents)
	_, err := Build(context.Background(), &Inputs{Rendered: rendered,
		Generators: generators(), Services: []string{"tasks/store", "tasks/worker"},
		Store: store, ReadPayloads: true, Modules: []string{"tasks"}})
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("Build = %v, want the in-scope empty key refused", err)
	}
}

// withEmpty returns documents with one property blanked.
func withEmpty(documents map[string]map[string]string, key, property string) map[string]map[string]string {
	documents[key][property] = ""
	return documents
}
