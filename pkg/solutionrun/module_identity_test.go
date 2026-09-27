package solutionrun

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// The module-principals workspace: a host whose accounts service is the
// registrar, and three modules beside the solution `app`.
//
//   - a: consumed by app under the facade prefix `a-api`; its api service
//     declares module-identity.
//   - b: consumed by nobody; its worker declares module-identity, its web
//     service does not.
//   - c: consumed by nobody and declares nothing.
const modulePrincipalsWorkspace = "testdata/module-principals"

func identityOf(t *testing.T, derived RunInputs, unique string) (prefix, secret string) {
	t.Helper()
	values := derived.Overrides[unique]
	prefix, secret = values[moduleIdentityPrefixEnvironmentVariable], values[moduleIdentitySecretEnvironmentVariable]
	if secret != "" && values[moduleRegistrationSecretEnvironmentVariable] != secret {
		t.Errorf("%s: the deprecated alias does not carry the identity secret", unique)
	}
	return prefix, secret
}

func assertNoIdentity(t *testing.T, derived RunInputs, uniques ...string) {
	t.Helper()
	for _, unique := range uniques {
		for _, key := range []string{moduleIdentityPrefixEnvironmentVariable, moduleIdentitySecretEnvironmentVariable, moduleRegistrationSecretEnvironmentVariable} {
			if _, exists := derived.Overrides[unique][key]; exists {
				t.Errorf("%s received %s, but it declares no module identity", unique, key)
			}
		}
	}
}

// A module principal needs its identity whether or not it registers a gateway
// facade. a federates one and b does not; both declare module-identity, so both
// get an identity and the host holds a digest for each — while only a, the
// facade, gets a registration secret. c declares nothing and gets nothing.
func TestDerivedRunInputsProvisionsEveryDeclaredModulePrincipal(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, modulePrincipalsWorkspace)
	app := loadTestModule(t, workspace, "app")

	derived, err := DerivedRunInputs(ctx, workspace, app, &resources.Service{Name: "backend"}, "app/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}

	aPrefix, aSecret := identityOf(t, derived, "a/api")
	if aPrefix != "a-api" || aSecret == "" {
		t.Fatalf("a/api identity = (%q, %q), want the facade prefix a-api and a secret", aPrefix, aSecret)
	}
	bPrefix, bSecret := identityOf(t, derived, "b/worker")
	if bPrefix != "b" || bSecret == "" {
		t.Fatalf("b/worker identity = (%q, %q), want prefix b and a secret", bPrefix, bSecret)
	}
	if aSecret == bSecret {
		t.Error("a and b were handed the same identity secret; either could mint the other's work context")
	}

	// Registration stays facade-only: the backend registers a-api and nothing else.
	registration := parsePairs(t, derived.Overrides["app/backend"][moduleRegistrationSecretsEnvironmentVariable])
	if len(registration) != 1 || registration["a-api"] == "" {
		t.Errorf("backend registration secrets = %v, want exactly a-api", registration)
	}
	declared := derived.WorkspaceConfigurations[federationConfigurationGroup]
	if got := parsePairs(t, declared[moduleRegistrationSecretsKey]); len(got) != 1 || got["a-api"] == "" {
		t.Errorf("registrar registration digests = %v, want exactly a-api", got)
	}

	// The host holds an identity digest for both principals, each matching the
	// plaintext its module presents.
	identities := parsePairs(t, declared[moduleIdentitySecretsKey])
	if len(identities) != 2 {
		t.Fatalf("registrar identity digests = %v, want a-api and b", identities)
	}
	if identities["a-api"] != moduleSecretDigest(aSecret) {
		t.Error("the registrar's a-api identity digest does not match a/api's secret")
	}
	if identities["b"] != moduleSecretDigest(bSecret) {
		t.Error("the registrar's b identity digest does not match b/worker's secret")
	}
	for _, value := range declared {
		for _, secret := range []string{aSecret, bSecret} {
			if strings.Contains(value, secret) {
				t.Error("the registrar received an identity plaintext; it must hold only digests")
			}
		}
	}

	// Undeclared: a sibling that does not declare, a module that declares
	// nothing, and the registrar itself.
	assertNoIdentity(t, derived, "b/web", "c/job", "host/accounts")
	for _, note := range derived.Notes {
		if note.Warning {
			t.Errorf("a fully wired composition reported a warning: %q", note.Message)
		}
	}
}

// The identity is not a solution run's side effect: running any service of the
// workspace provisions it, so a principal booted beside its host without a
// solution entry still authenticates. The facade prefix a is consumed under is
// still its identity; nothing registers a facade, so no registration secret is
// minted at all.
func TestDerivedRunInputsProvisionsDeclaredIdentitiesOutsideASolutionRun(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, modulePrincipalsWorkspace)
	host := loadTestModule(t, workspace, "host")

	derived, err := DerivedRunInputs(ctx, workspace, host, &resources.Service{Name: "accounts"}, "host/accounts")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	if prefix, secret := identityOf(t, derived, "a/api"); prefix != "a-api" || secret == "" {
		t.Errorf("a/api identity = (%q, %q), want the facade prefix a-api", prefix, secret)
	}
	if prefix, secret := identityOf(t, derived, "b/worker"); prefix != "b" || secret == "" {
		t.Errorf("b/worker identity = (%q, %q), want prefix b", prefix, secret)
	}
	declared := derived.WorkspaceConfigurations[federationConfigurationGroup]
	if _, exists := declared[moduleRegistrationSecretsKey]; exists {
		t.Errorf("declared registration digests %q with no facade to register", declared[moduleRegistrationSecretsKey])
	}
	if got := parsePairs(t, declared[moduleIdentitySecretsKey]); len(got) != 2 || got["a-api"] == "" || got["b"] == "" {
		t.Errorf("registrar identity digests = %v, want a-api and b", got)
	}
	for service, values := range derived.Overrides {
		if _, leaked := values[moduleRegistrationSecretsEnvironmentVariable]; leaked {
			t.Errorf("%s received registration secrets in a run with no solution entry", service)
		}
	}
	assertNoIdentity(t, derived, "b/web", "c/job", "host/accounts")
}

// With no registrar, nothing can admit an identity: the run mints none and says
// which declared modules are left without one.
func TestDerivedRunInputsWithholdsDeclaredIdentitiesWithoutARegistrar(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, modulePrincipalsWorkspace)
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "host" })
	b := loadTestModule(t, workspace, "b")

	derived, err := DerivedRunInputs(ctx, workspace, b, &resources.Service{Name: "worker"}, "b/worker")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	if derived.Overrides != nil || derived.WorkspaceConfigurations != nil {
		t.Fatalf("provisioned inputs with no registrar to admit them: %+v", derived)
	}
	var warned bool
	for _, note := range derived.Notes {
		if note.Warning && strings.Contains(note.Message, "a, b") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("withholding the declared identities was not reported: %+v", derived.Notes)
	}
}

// Every root of a run mints its own identity for a module, and the registrar
// keeps the first root's digest. The module's services must keep the first
// root's plaintext with it, or they present a secret whose digest was never
// declared.
func TestMergeKeepsTheFirstRootsModuleIdentity(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, modulePrincipalsWorkspace)
	app := loadTestModule(t, workspace, "app")
	host := loadTestModule(t, workspace, "host")

	first, err := DerivedRunInputs(ctx, workspace, app, &resources.Service{Name: "backend"}, "app/backend")
	if err != nil {
		t.Fatal(err)
	}
	second, err := DerivedRunInputs(ctx, workspace, host, &resources.Service{Name: "accounts"}, "host/accounts")
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(first, second)
	identities := parsePairs(t, merged.WorkspaceConfigurations[federationConfigurationGroup][moduleIdentitySecretsKey])
	for unique, prefix := range map[string]string{"a/api": "a-api", "b/worker": "b"} {
		_, secret := identityOf(t, merged, unique)
		if _, want := identityOf(t, first, unique); secret != want {
			t.Errorf("%s: a later root replaced the first root's identity", unique)
		}
		if identities[prefix] != moduleSecretDigest(secret) {
			t.Errorf("%s presents a secret whose digest the registrar does not hold for %s", unique, prefix)
		}
	}
}
