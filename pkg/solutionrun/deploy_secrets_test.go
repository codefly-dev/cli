package solutionrun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// federationWorkspaceWithSolutionRegistration copies the federation testdata
// and has the wiki backend declare the solution-registration group, which is
// what makes a solution's own secret a stored key.
func federationWorkspaceWithSolutionRegistration(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/solution-federation")); err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(dir, "services", "backend", "service.codefly.yaml")
	data, err := os.ReadFile(backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backend, append(data, []byte("    - solution-registration\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Every key a render references for the federation is derived, and from the
// credentials the run would mint for the same exchange: the entry's map, the
// consumed module's identity under both carriers, and the registrar's digests.
func TestDeployedFederationSecretsNamesEveryReferencedKey(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, federationWorkspaceWithSolutionRegistration(t))

	secrets, err := DeployedFederationSecrets(ctx, workspace)
	if err != nil {
		t.Fatalf("DeployedFederationSecrets: %v", err)
	}
	registration := Credential{Kind: ModuleRegistration, Identity: "documents"}
	identity := Credential{Kind: ModuleIdentity, Identity: "documents"}
	solution := Credential{Kind: SolutionRegistration, Identity: "wiki"}

	want := map[string]map[string]SecretDerivation{
		"wiki/backend": {
			moduleRegistrationSecretsEnvironmentVariable:                         {Credentials: []Credential{registration}, Encoded: true},
			workspaceSecretKey(solutionRegistrationConfigurationGroup, "SECRET"): {Credentials: []Credential{solution}},
		},
		"documents/api": {
			moduleIdentitySecretEnvironmentVariable:     {Credentials: []Credential{identity}},
			moduleRegistrationSecretEnvironmentVariable: {Credentials: []Credential{identity}},
		},
		"documents/worker": {
			moduleIdentitySecretEnvironmentVariable:     {Credentials: []Credential{identity}},
			moduleRegistrationSecretEnvironmentVariable: {Credentials: []Credential{identity}},
		},
	}
	for _, registrar := range []string{"host/accounts"} {
		want[registrar] = map[string]SecretDerivation{
			"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__FEDERATION__MODULE_REGISTRATION_SECRETS":   {Credentials: []Credential{registration}, Encoded: true, Digest: true},
			"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__FEDERATION__MODULE_IDENTITY_SECRETS":       {Credentials: []Credential{identity}, Encoded: true, Digest: true},
			"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__FEDERATION__SOLUTION_REGISTRATION_SECRETS": {Credentials: []Credential{solution}, Encoded: true, Digest: true},
		}
	}
	for unique, keys := range want {
		for key, derivation := range keys {
			got, ok := secrets.For(unique, key)
			if !ok {
				t.Errorf("%s %s: not derived", unique, key)
				continue
			}
			if !reflect.DeepEqual(got, derivation) {
				t.Errorf("%s %s = %+v, want %+v", unique, key, got, derivation)
			}
		}
	}
	credentials := secrets.Credentials()
	if len(credentials) != 3 || !slices.Contains(credentials, identity) || !slices.Contains(credentials, registration) || !slices.Contains(credentials, solution) {
		t.Errorf("credentials = %v, want exactly the three the exchange uses", credentials)
	}
}

// The values derived for both ends of one exchange agree: the digest the
// registrar declares is the digest of the plaintext the consumer presents, in
// the encoding the run's own tests parse.
func TestDeployedFederationSecretsDerivesAgreeingEnds(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, federationWorkspaceWithSolutionRegistration(t))
	secrets, err := DeployedFederationSecrets(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	plaintexts := map[Credential]string{}
	for _, credential := range secrets.Credentials() {
		plaintexts[credential] = MintCredential()
	}
	value := func(unique, key string) string {
		t.Helper()
		derivation, ok := secrets.For(unique, key)
		if !ok {
			t.Fatalf("%s %s not derived", unique, key)
		}
		derived, err := derivation.Value(plaintexts)
		if err != nil {
			t.Fatal(err)
		}
		return derived
	}
	presented := parsePairs(t, value("wiki/backend", moduleRegistrationSecretsEnvironmentVariable))
	declared := parsePairs(t, value("host/accounts", "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__FEDERATION__MODULE_REGISTRATION_SECRETS"))
	if moduleSecretDigest(presented["documents"]) != declared["documents"] {
		t.Error("registrar's registration digest does not admit the backend's secret")
	}
	identity := value("documents/api", moduleIdentitySecretEnvironmentVariable)
	identities := parsePairs(t, value("host/accounts", "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__FEDERATION__MODULE_IDENTITY_SECRETS"))
	if moduleSecretDigest(identity) != identities["documents"] {
		t.Error("registrar's identity digest does not admit the module's secret")
	}
	if identity == presented["documents"] {
		t.Error("identity and registration secrets are the same value")
	}
	solution := value("wiki/backend", workspaceSecretKey(solutionRegistrationConfigurationGroup, "SECRET"))
	solutions := parsePairs(t, value("host/accounts", "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__FEDERATION__SOLUTION_REGISTRATION_SECRETS"))
	if moduleSecretDigest(solution) != solutions["wiki"] {
		t.Error("registrar's solution digest does not admit the solution's secret")
	}
}

// A stored plaintext is recovered, so a credential already in the store is
// reused; a digest yields nothing, and an undeclared identity is not trusted.
func TestSecretDerivationPlaintexts(t *testing.T) {
	documents := Credential{Kind: ModuleRegistration, Identity: "documents"}
	model := Credential{Kind: ModuleRegistration, Identity: "model"}
	encoded := SecretDerivation{Credentials: []Credential{documents, model}, Encoded: true}
	got := encoded.Plaintexts("documents:aaa,stranger:bbb")
	if !reflect.DeepEqual(got, map[Credential]string{documents: "aaa"}) {
		t.Errorf("Plaintexts = %v, want only the declared documents entry", got)
	}
	if got := (SecretDerivation{Credentials: []Credential{documents}, Encoded: true, Digest: true}).Plaintexts("documents:abc"); got != nil {
		t.Errorf("a digest yielded plaintexts %v", got)
	}
	single := SecretDerivation{Credentials: []Credential{documents}}
	if got := single.Plaintexts("secret"); got[documents] != "secret" {
		t.Errorf("single Plaintexts = %v", got)
	}
	if _, err := encoded.Value(map[Credential]string{documents: "aaa"}); err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("Value with a missing credential = %v, want an error naming it", err)
	}
}

// Without a registrar nothing could admit a credential, so nothing is derived —
// exactly as a render references nothing.
func TestDeployedFederationSecretsDerivesNothingWithoutARegistrar(t *testing.T) {
	ctx := context.Background()
	dir := federationWorkspaceWithSolutionRegistration(t)
	for _, service := range []string{"accounts", "gateway"} {
		path := filepath.Join(dir, "modules", "host", "services", service, "service.codefly.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "    - federation\n", "")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	secrets, err := DeployedFederationSecrets(ctx, loadTestWorkspace(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets.Services) != 0 {
		t.Errorf("derived %v without a registrar", secrets.Services)
	}
}

// Without a registrar nothing can admit a federation credential, so deriving
// nothing is right — but saying nothing leaves `deploy secrets` reporting every
// federation key as one the operator must type, with the actual cause unnamed.
// The run and the render both warn here; so does this.
func TestDeployedFederationSecretsWarnsWithoutARegistrar(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "host" })

	secrets, err := DeployedFederationSecrets(ctx, workspace)
	if err != nil {
		t.Fatalf("DeployedFederationSecrets: %v", err)
	}
	if len(secrets.Services) != 0 {
		t.Errorf("derived %v without a registrar", secrets.Services)
	}
	if len(secrets.Notes) != 1 || !secrets.Notes[0].Warning {
		t.Fatalf("notes = %+v, want one warning naming the missing group", secrets.Notes)
	}
	if !strings.Contains(secrets.Notes[0].Message, federationConfigurationGroup) {
		t.Errorf("warning %q does not name the %q group", secrets.Notes[0].Message, federationConfigurationGroup)
	}
}

// Identities is how a caller rewriting an `identity:value,…` value sees what the
// store admits today: an identity the derivation no longer covers would be
// dropped by the rewrite, de-authorizing whatever holds it.
func TestSecretDerivationIdentities(t *testing.T) {
	encoded := SecretDerivation{Credentials: []Credential{{Kind: ModuleRegistration, Identity: "documents"}}, Encoded: true, Digest: true}
	if got := encoded.Identities("documents:aaa, legacy:bbb ,documents:ccc"); !reflect.DeepEqual(got, []string{"documents", "legacy"}) {
		t.Errorf("Identities = %v, want each identity once in stored order", got)
	}
	for _, malformed := range []string{"", "documents:", ":bbb", "documents"} {
		if got := encoded.Identities(malformed); len(got) != 0 {
			t.Errorf("Identities(%q) = %v, want none: a malformed entry admits nothing", malformed, got)
		}
	}
	single := SecretDerivation{Credentials: []Credential{{Kind: SolutionRegistration, Identity: "wiki"}}}
	if got := single.Identities("wiki:not-encoded"); got != nil {
		t.Errorf("Identities = %v, want none: a value that is not of the encoded form declares no identities", got)
	}
}
