package deploysecrets

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

// The obin-blue shape: a ClickHouse server whose generated password and user
// are filed in its own remote key, and a consumer whose remote key holds the
// connection URL an operator stored by hand (no producer template), which the
// plan alone reports as `require`.
const (
	evidencePassword   = "CODEFLY__SERVICE_SECRET_CONFIGURATION__TRACING__EVIDENCE__CLICKHOUSE__CLICKHOUSE_PASSWORD"
	evidenceUser       = "CODEFLY__SERVICE_SECRET_CONFIGURATION__TRACING__EVIDENCE__CLICKHOUSE__CLICKHOUSE_USER"
	evidenceConnection = "CODEFLY__SERVICE_SECRET_CONFIGURATION__TRACING__EVIDENCE__CLICKHOUSE__CONNECTION"
	evidenceKey        = "blue-tracing-evidence"
	consumerKey        = "blue-tracing-tracing"
	oldPassword        = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func rotationFixture(consumerServices ...string) (gitops.RenderedEnvironment, []environments.EnvironmentSecretGenerator, *fakeStore) {
	if len(consumerServices) == 0 {
		consumerServices = []string{"tracing/tracing"}
	}
	store := environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
		{Store: store, RemoteKey: evidenceKey, Services: []string{"tracing/evidence"}, Properties: filed(evidencePassword, evidenceUser)},
		{Store: store, RemoteKey: consumerKey, Services: consumerServices, Properties: filed(evidenceConnection)},
	}}
	generators := []environments.EnvironmentSecretGenerator{{Scope: environments.SecretGeneratorScopeService,
		Configuration: "clickhouse", Services: []string{"tracing/evidence"}, Keys: []string{"CLICKHOUSE_PASSWORD"}}, {Scope: environments.SecretGeneratorScopeService,
		Configuration: "clickhouse", Services: []string{"tracing/evidence"}, Keys: []string{"CLICKHOUSE_USER"}, Format: environments.SecretGeneratorFormatIdentifier}}
	return rendered, generators, newFakeStore(map[string]map[string]string{
		evidenceKey: {evidencePassword: oldPassword, evidenceUser: "evidence"},
		consumerKey: {evidenceConnection: "clickhouse://evidence:" + oldPassword + "@evidence.tracing.svc:9000/evidence?secure=false"},
	})
}

func rotationPlan(t *testing.T, rendered gitops.RenderedEnvironment, generators []environments.EnvironmentSecretGenerator, store Store, rotate ...string) (*Plan, error) {
	t.Helper()
	return Build(context.Background(), &Inputs{Rendered: rendered, Generators: generators, Store: store, ReadPayloads: true, Rotate: rotate})
}

// assertConsistent checks, without printing either value, that the stored
// connection's password is the stored password and everything else is as before.
func assertConsistent(t *testing.T, store *fakeStore) {
	t.Helper()
	parsed, err := url.Parse(store.documents[consumerKey][evidenceConnection])
	if err != nil {
		t.Fatal("the stored connection is no longer a URL")
	}
	password, _ := parsed.User.Password()
	if password != store.documents[evidenceKey][evidencePassword] {
		t.Error("the stored connection's password is not the stored password")
	}
	if parsed.User.Username() != "evidence" || parsed.Host != "evidence.tracing.svc:9000" || parsed.Path != "/evidence" || parsed.RawQuery != "secure=false" {
		t.Error("rewriting the connection changed more than its password")
	}
}

func TestRotationKeyResolvesTheGeneratorSpelling(t *testing.T) {
	for selector, want := range map[string]string{
		"tracing/evidence/clickhouse:CLICKHOUSE_PASSWORD":           evidencePassword,
		"tracing/evidence/clickhouse:clickhouse-password":           evidencePassword,
		"internal-auth:codefly-internal-token":                      internalToken,
		evidencePassword:                                            evidencePassword,
		"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__IDENTITY__SECRET": "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__IDENTITY__SECRET",
	} {
		if got, err := RotationKey(selector); err != nil || got != want {
			t.Errorf("RotationKey(%q) = %q, %v; want %q", selector, got, err, want)
		}
	}
	for _, selector := range []string{"", "PASSWORD", "tracing/evidence:PASSWORD", "tracing//clickhouse:PASSWORD", "clickhouse:"} {
		if _, err := RotationKey(selector); err == nil {
			t.Errorf("RotationKey(%q) accepted a malformed selector", selector)
		}
	}
}

// A generated key rotates in every holder, the connection derived from it is
// rewritten in the same apply, the result names versions and properties only,
// and a plain re-run afterwards writes nothing.
func TestRotateReplacesAGeneratedKeyAndTheConnectionDerivedFromIt(t *testing.T) {
	ctx := context.Background()
	rendered, generators, store := rotationFixture()
	plan, err := rotationPlan(t, rendered, generators, store, evidencePassword)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, check := range []struct {
		key, property string
		action        Action
	}{
		{evidenceKey, evidencePassword, ActionRotate},
		{evidenceKey, evidenceUser, ActionKeep},
		{consumerKey, evidenceConnection, ActionRotate},
	} {
		if got := propertyPlan(t, plan, check.key, check.property); got.Action != check.action {
			t.Errorf("%s#%s = %s (%s), want %s", check.key, check.property, got.Action, got.Source, check.action)
		}
	}
	reported := fmt.Sprintf("%+v %v %v", plan.Secrets, plan.Changes(), plan.Required())
	written, err := plan.Apply(ctx, store, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rotated := store.documents[evidenceKey][evidencePassword]
	if rotated == oldPassword || len(rotated) != 64 {
		t.Fatal("the password was not replaced by a new 32-byte hex value")
	}
	if strings.Contains(reported, rotated) || strings.Contains(reported, oldPassword) {
		t.Fatal("the plan's report carries a value")
	}
	assertConsistent(t, store)
	want := []Written{
		{Key: evidenceKey, Properties: []string{evidencePassword}, Version: "1"},
		{Key: consumerKey, Properties: []string{evidenceConnection}, Version: "1"},
	}
	if !slices.EqualFunc(written, want, func(a, b Written) bool {
		return a.Key == b.Key && a.Version == b.Version && !a.Restored && slices.Equal(a.Properties, b.Properties)
	}) {
		t.Errorf("written = %+v, want %+v", written, want)
	}

	again, err := rotationPlan(t, rendered, generators, store)
	if err != nil {
		t.Fatal(err)
	}
	if changes := again.Changes(); len(changes) != 0 {
		t.Errorf("a plain re-run after the rotation would write %v", changes)
	}
}

// Only a key codefly generates is rotated: a supplied key, a configured one and
// a signing key are each refused with their reason, before anything is written.
func TestRotateRefusesAKeyCodeflyDoesNotProduce(t *testing.T) {
	rendered, generators, store := rotationFixture()
	if _, err := rotationPlan(t, rendered, generators, store, evidenceConnection); err == nil ||
		!strings.Contains(err.Error(), "service-secrets.generate does not declare it") {
		t.Errorf("rotating a supplied key = %v, want a refusal saying it is not generated", err)
	}
	_, err := Build(context.Background(), &Inputs{Rendered: rendered, Generators: generators, Store: store, ReadPayloads: true,
		Rotate:     []string{evidencePassword},
		Configured: map[string]ConfiguredValue{evidencePassword: {File: "configurations/blue/clickhouse.secret.env", value: oldPassword}}})
	if err == nil || !strings.Contains(err.Error(), "configurations/blue/clickhouse.secret.env") {
		t.Errorf("rotating a configured key = %v, want a refusal naming the file", err)
	}
	signing := []environments.EnvironmentSecretGenerator{{Scope: environments.SecretGeneratorScopeService, Configuration: "clickhouse",
		Services: []string{"tracing/evidence"}, Keys: []string{"CLICKHOUSE_PASSWORD"}, Format: environments.SecretGeneratorFormatEd25519PKCS8}}
	if _, err := rotationPlan(t, rendered, signing, store, evidencePassword); err == nil || !strings.Contains(err.Error(), "signing key") {
		t.Errorf("rotating a signing key = %v, want a refusal", err)
	}
	if _, err := rotationPlan(t, rendered, generators, store, evidenceUser); err == nil || !strings.Contains(err.Error(), "identifier") {
		t.Errorf("rotating an identifier = %v, want a refusal: a new user name in the store renames nobody", err)
	}
	if len(store.writes) != 0 {
		t.Errorf("a refused rotation wrote %v", store.writes)
	}
}

// A stored value embedding the old one in a shape codefly cannot recompute is
// refused by name instead of being left stale; so is a derived value outside
// --module.
func TestRotateRefusesADerivedValueItCannotKeepConsistent(t *testing.T) {
	rendered, generators, store := rotationFixture()
	store.documents[consumerKey][evidenceConnection] = "host=evidence password=" + oldPassword
	_, err := rotationPlan(t, rendered, generators, store, evidencePassword)
	if err == nil || !strings.Contains(err.Error(), consumerKey+"#"+evidenceConnection) {
		t.Errorf("an unrecomputable derived value = %v, want a refusal naming it", err)
	}
	if err != nil && strings.Contains(err.Error(), oldPassword) {
		t.Fatal("the refusal carries the value")
	}

	rendered, generators, store = rotationFixture("observer/api")
	_, err = Build(context.Background(), &Inputs{Rendered: rendered, Generators: generators, Store: store, ReadPayloads: true,
		Rotate: []string{evidencePassword}, Modules: []string{"tracing"}})
	if err == nil || !strings.Contains(err.Error(), "outside --module") {
		t.Errorf("a derived value outside the scope = %v, want a refusal", err)
	}
}

// --dry-run --metadata-only reads no value and shows the rotation by property
// name; it cannot be applied.
func TestRotateMetadataOnlyShowsRowsWithoutReading(t *testing.T) {
	rendered, generators, store := rotationFixture()
	plan, err := Build(context.Background(), &Inputs{Rendered: rendered, Generators: generators, Store: store, Rotate: []string{evidencePassword}})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.reads) != 0 {
		t.Errorf("a metadata-only plan read %v", store.reads)
	}
	if got := propertyPlan(t, plan, evidenceKey, evidencePassword); got.Action != ActionRotate {
		t.Errorf("rotated property = %s, want rotate", got.Action)
	}
	if !plan.Unverified() || len(plan.writes) != 0 {
		t.Error("a metadata-only rotation is applicable")
	}
	if strings.Contains(fmt.Sprintf("%+v", plan.Secrets), oldPassword) {
		t.Fatal("the plan carries a value")
	}
}

// A rotation failing part-way restores what it already wrote as new versions,
// so the source and its derived connection still agree; the same command then
// converges.
func TestRotateFailingPartWayLeavesNothingHalfRotated(t *testing.T) {
	ctx := context.Background()
	rendered, generators, store := rotationFixture()
	before := maps.Clone(store.documents[evidenceKey])
	store.failWrite = map[string]bool{consumerKey: true}
	plan, err := rotationPlan(t, rendered, generators, store, evidencePassword)
	if err != nil {
		t.Fatal(err)
	}
	written, err := plan.Apply(ctx, store, false)
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("Apply = %v, want the failure and the restore reported", err)
	}
	if !maps.Equal(store.documents[evidenceKey], before) {
		t.Error("the rotated password was left in place after its connection failed to write")
	}
	if len(written) != 2 || !written[1].Restored || written[1].Key != evidenceKey || written[1].Version != "2" {
		t.Errorf("written = %+v, want the rotation then its restore as version 2", written)
	}
	assertConsistent(t, store)

	store.failWrite = nil
	retry, err := rotationPlan(t, rendered, generators, store, evidencePassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = retry.Apply(ctx, store, false); err != nil {
		t.Fatal(err)
	}
	if store.documents[evidenceKey][evidencePassword] == oldPassword {
		t.Error("the retried rotation did not rotate")
	}
	assertConsistent(t, store)
}

// A configuration read through two stores cannot be rotated by one plan.
func TestCheckRotationStoresRefusesAConfigurationSpanningStores(t *testing.T) {
	rendered, _, _ := rotationFixture()
	other := rendered
	other.Secrets = []gitops.RenderedServiceSecret{rendered.Secrets[1]}
	other.Secrets[0].Store.Name = "consumer-secrets"
	one := rendered
	one.Secrets = rendered.Secrets[:1]
	if err := CheckRotationStores([]gitops.RenderedEnvironment{one, other}, evidencePassword); err == nil {
		t.Error("a configuration spanning two stores was accepted")
	}
	if err := CheckRotationStores([]gitops.RenderedEnvironment{rendered}, evidencePassword); err != nil {
		t.Errorf("one store: %v", err)
	}
}

// A base64 password holds '/' and '+', which a URL's userinfo percent-encodes.
// The derived connection is found by its DECODED password, rewritten with the
// new one encoded the same way, and found again by the next rotation.
func TestRotateFindsAPercentEncodedPassword(t *testing.T) {
	const encoded = "ab/cd+ef/gh+ij=="
	for name, stored := range map[string]string{
		"userinfo escaping": "clickhouse://evidence:" + escapedPassword(encoded) + "@evidence.tracing.svc:9000/evidence?secure=false",
		"query escaping":    "clickhouse://evidence:" + url.QueryEscape(encoded) + "@evidence.tracing.svc:9000/evidence?secure=false",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			rendered, generators, store := rotationFixture()
			generators[0].Format = environments.SecretGeneratorFormatBase64
			store.documents[evidenceKey][evidencePassword] = encoded
			store.documents[consumerKey][evidenceConnection] = stored
			for range 2 {
				plan, err := rotationPlan(t, rendered, generators, store, evidencePassword)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				if got := propertyPlan(t, plan, consumerKey, evidenceConnection); got.Action != ActionRotate {
					t.Fatalf("the percent-encoded connection = %s, want rotate", got.Action)
				}
				if _, err = plan.Apply(ctx, store, false); err != nil {
					t.Fatal(err)
				}
				assertConsistent(t, store)
			}
			if store.documents[evidenceKey][evidencePassword] == encoded {
				t.Error("the password was not rotated")
			}
		})
	}
}

// A write that reports failure after it landed is restored too, and the error
// says what the store holds.
func TestRotateRestoresAFailedWriteThatLanded(t *testing.T) {
	rendered, generators, store := rotationFixture()
	before := maps.Clone(store.documents[consumerKey])
	store.landThenFail = map[string]bool{consumerKey: true}
	plan, err := rotationPlan(t, rendered, generators, store, evidencePassword)
	if err != nil {
		t.Fatal(err)
	}
	_, err = plan.Apply(context.Background(), store, false)
	if err == nil || !strings.Contains(err.Error(), "the 2 remote keys this rotation wrote were restored") {
		t.Fatalf("Apply = %v, want both keys reported restored", err)
	}
	if !maps.Equal(store.documents[consumerKey], before) || store.documents[evidenceKey][evidencePassword] != oldPassword {
		t.Error("a landed write was left rotated")
	}
	assertConsistent(t, store)
}

// A key someone wrote after the plan read it is never overwritten: the apply
// refuses by name, and what the rotation already wrote is restored.
func TestRotateRefusesAKeyThatMovedSinceThePlan(t *testing.T) {
	rendered, generators, store := rotationFixture()
	plan, err := rotationPlan(t, rendered, generators, store, evidencePassword)
	if err != nil {
		t.Fatal(err)
	}
	store.versions = map[string]int{consumerKey: 5}
	_, err = plan.Apply(context.Background(), store, false)
	if err == nil || !strings.Contains(err.Error(), consumerKey+" changed after the plan read it") {
		t.Fatalf("Apply = %v, want the moved key refused by name", err)
	}
	if store.documents[evidenceKey][evidencePassword] != oldPassword || slices.Contains(store.writes[:1], consumerKey) {
		t.Error("the rotation was left half-applied or overwrote the moved key")
	}
}
