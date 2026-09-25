package deploysecrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/solutionrun"
)

// fakeStore is an in-memory store that records every call, so a test can prove
// what was read and written.
type fakeStore struct {
	documents map[string]map[string]string
	// empty are keys that exist with no enabled version.
	empty  map[string]bool
	reads  []string
	writes []string
}

func newFakeStore(documents map[string]map[string]string) *fakeStore {
	return &fakeStore{documents: documents, empty: map[string]bool{}}
}

func (store *fakeStore) Name() string { return "fake" }

func (store *fakeStore) Describe(_ context.Context, key string) (Description, error) {
	if store.empty[key] {
		return Description{Exists: true}, nil
	}
	_, exists := store.documents[key]
	return Description{Exists: exists, HasVersion: exists}, nil
}

func (store *fakeStore) Read(_ context.Context, key string) (map[string]string, error) {
	store.reads = append(store.reads, key)
	document, ok := store.documents[key]
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
	registrations   = "CODEFLY__MODULE_REGISTRATION_SECRETS"
	solutionSecret  = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__SOLUTION_REGISTRATION__SECRET"
	registrarDigest = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__FEDERATION__MODULE_REGISTRATION_SECRETS"
	solutionDigest  = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__FEDERATION__SOLUTION_REGISTRATION_SECRETS"
)

var (
	documentsRegistration = solutionrun.Credential{Kind: solutionrun.ModuleRegistration, Identity: "documents"}
	wikiSolution          = solutionrun.Credential{Kind: solutionrun.SolutionRegistration, Identity: "wiki"}
	notesSolution         = solutionrun.Credential{Kind: solutionrun.SolutionRegistration, Identity: "notes"}
)

// fixture is a composition in the shape the staging cell has: a host whose
// accounts service holds the digests, two solutions consuming the documents
// prefix, and a module with a database.
func fixture() (gitops.RenderedEnvironment, solutionrun.DeployedSecrets) {
	store := environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: store, RemoteKey: "host-accounts", Services: []string{"host/accounts"}, Properties: []string{clientSecret, internalToken, registrarDigest, solutionDigest}},
		{Store: store, RemoteKey: "notes-backend", Services: []string{"notes/backend"}, Properties: []string{registrations, internalToken, solutionSecret}},
		{Store: store, RemoteKey: "tasks-store", Services: []string{"tasks/store"}, Properties: []string{migration, storePassword}},
		{Store: store, RemoteKey: "tasks-worker", Services: []string{"tasks/worker"}, Properties: []string{storeConnection}},
		{Store: store, RemoteKey: "wiki-backend", Services: []string{"wiki/backend"}, Properties: []string{registrations, internalToken, solutionSecret}},
	}}
	registration := solutionrun.SecretDerivation{Credentials: []solutionrun.Credential{documentsRegistration}, Encoded: true}
	federation := solutionrun.DeployedSecrets{Services: map[string]map[string]solutionrun.SecretDerivation{
		"wiki/backend":  {registrations: registration, solutionSecret: {Credentials: []solutionrun.Credential{wikiSolution}}},
		"notes/backend": {registrations: registration, solutionSecret: {Credentials: []solutionrun.Credential{notesSolution}}},
		"host/accounts": {
			registrarDigest: {Credentials: []solutionrun.Credential{documentsRegistration}, Encoded: true, Digest: true},
			solutionDigest:  {Credentials: []solutionrun.Credential{notesSolution, wikiSolution}, Encoded: true, Digest: true},
		},
	}}
	return rendered, federation
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// seeded is the store as a hand-seeded cell leaves it: the host and one
// solution wired, a second solution and a new module never seeded.
func seeded() map[string]map[string]string {
	return map[string]map[string]string{
		"host-accounts": {
			clientSecret:    "external-client-secret",
			internalToken:   "shared-internal-token",
			registrarDigest: "documents:" + digest("documents-registration"),
			solutionDigest:  "wiki:" + digest("wiki-solution"),
		},
		"wiki-backend": {
			registrations:  "documents:documents-registration",
			internalToken:  "shared-internal-token",
			solutionSecret: "wiki-solution",
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

// The staging shape: a second solution joins a hand-seeded cell. It reuses the
// registration secret the first solution holds, gets its own solution secret,
// is handed the shared token the others hold, and the registrar's solution
// digests are rewritten to admit it — while nothing already stored is rotated.
func TestPlanSeedsANewSolutionConsistentlyWithTheSeededCell(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	store := newFakeStore(seeded())

	plan, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, check := range []struct {
		key, property string
		action        Action
	}{
		{"host-accounts", clientSecret, ActionKeep},
		{"host-accounts", registrarDigest, ActionKeep},
		{"host-accounts", solutionDigest, ActionUpdate},
		{"wiki-backend", registrations, ActionKeep},
		{"wiki-backend", solutionSecret, ActionKeep},
		{"notes-backend", registrations, ActionDerive},
		{"notes-backend", solutionSecret, ActionDerive},
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
	// The registrar's digests go last, after every plaintext they admit.
	if len(written) == 0 || written[len(written)-1] != "host-accounts" {
		t.Errorf("written %v, want host-accounts last", written)
	}
	notes := store.documents["notes-backend"]
	if notes[registrations] != "documents:documents-registration" {
		t.Error("the new solution was not handed the registration secret the store already holds")
	}
	if notes[internalToken] != "shared-internal-token" {
		t.Error("the shared token was not propagated")
	}
	host := store.documents["host-accounts"]
	if host[clientSecret] != "external-client-secret" || host[registrarDigest] != "documents:"+digest("documents-registration") {
		t.Error("an already-stored value was rewritten")
	}
	solutions := map[string]string{}
	for _, entry := range strings.Split(host[solutionDigest], ",") {
		identity, value, _ := strings.Cut(entry, ":")
		solutions[identity] = value
	}
	if solutions["wiki"] != digest("wiki-solution") || solutions["notes"] != digest(notes[solutionSecret]) {
		t.Errorf("registrar's solution digests %q do not admit both solutions' stored secrets", host[solutionDigest])
	}
	if store.documents["wiki-backend"][solutionSecret] != "wiki-solution" {
		t.Error("the seeded solution's secret was rotated")
	}
	if len(store.documents["tasks-store"][storePassword]) != 64 {
		t.Error("the declared generator did not produce a 32-byte hex value")
	}
	if _, present := store.documents["tasks-store"][migration]; present {
		t.Error("a required property was written with no source")
	}

	// Re-planning the applied store changes nothing but what still has no source.
	again, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: generators(),
		Services: []string{"tasks/store", "tasks/worker"}, Store: store, ReadPayloads: true})
	if err != nil {
		t.Fatal(err)
	}
	if changes := again.Changes(); len(changes) != 0 {
		t.Errorf("a second plan would write %v", changes)
	}
}

// A plan is reported by name and source only: no value — stored, derived or
// generated — appears anywhere in it.
func TestPlanNeverCarriesAValue(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	documents := seeded()
	store := newFakeStore(documents)
	plan, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: generators(),
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
	reported := fmt.Sprintf("%+v %+v %+v %+v %v", plan.Secrets, plan.Credentials, plan.Notes, plan.Changes(), plan.Required())
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
	rendered, federation := fixture()
	store := newFakeStore(seeded())
	plan, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: generators(),
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

// Two keys carrying one credential with different values no longer federate:
// the plan refuses instead of picking one.
func TestPlanRefusesACredentialHeldWithTwoValues(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	documents := seeded()
	documents["notes-backend"] = map[string]string{registrations: "documents:another-value"}
	_, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Store: newFakeStore(documents), ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), documentsRegistration.String()) {
		t.Fatalf("Build = %v, want a refusal naming %s", err, documentsRegistration)
	}
	if strings.Contains(err.Error(), "another-value") || strings.Contains(err.Error(), "documents-registration") {
		t.Error("the refusal quotes a value")
	}
}

// A configuration value is one value: two keys holding it differently cannot be
// propagated from.
func TestPlanRefusesToPropagateAConflictingValue(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	documents := seeded()
	documents["wiki-backend"][internalToken] = "a-different-token"
	_, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Store: newFakeStore(documents), ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), internalToken) {
		t.Fatalf("Build = %v, want a refusal naming the key", err)
	}
}

// With nothing stored at all, every credential is minted once and every key
// deriving from it agrees; a generated shared value is one value everywhere.
func TestPlanOnAnEmptyStoreMintsOnceAndAgrees(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	store := newFakeStore(map[string]map[string]string{})
	store.empty["host-accounts"] = true
	plan, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: generators(),
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
	if wiki[registrations] == "" || wiki[registrations] != notes[registrations] {
		t.Error("two solutions consuming one prefix hold different registration secrets")
	}
	if wiki[internalToken] == "" || wiki[internalToken] != notes[internalToken] || host[internalToken] != wiki[internalToken] {
		t.Error("a generated shared value differs between the keys reading it")
	}
	_, secret, _ := strings.Cut(wiki[registrations], ":")
	if host[registrarDigest] != "documents:"+digest(secret) {
		t.Error("the registrar's digest does not admit the minted registration secret")
	}
	if wiki[solutionSecret] == notes[solutionSecret] {
		t.Error("two solutions share one solution secret")
	}
	if !slices.Contains(store.writes, "host-accounts") {
		t.Error("a key with no enabled version was not written")
	}
}

func TestGeneratorCoveringAKeyTwiceIsRefused(t *testing.T) {
	ctx := context.Background()
	rendered, federation := fixture()
	duplicate := append(generators(), environments.EnvironmentSecretGenerator{
		Scope: environments.SecretGeneratorScopeService, Configuration: "postgres", Services: []string{"tasks/store"}, Keys: []string{"POSTGRES_PASSWORD"}})
	_, err := Build(ctx, &Inputs{Rendered: rendered, Federation: federation, Generators: duplicate,
		Services: []string{"tasks/store"}, Store: newFakeStore(seeded()), ReadPayloads: true})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("Build = %v, want a refusal", err)
	}
}
