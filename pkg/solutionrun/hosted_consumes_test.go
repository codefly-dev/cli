package solutionrun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codefly-dev/core/solution/manifest"
)

// federationWorkspaceConsumingTheHost copies the federation testdata and has the
// wiki also consume the registrar module's own accounts API under the facade
// prefix "accounts" — the shape a solution calling the host's accounts service
// has — optionally as its only consumed API.
func federationWorkspaceConsumingTheHost(t *testing.T, only bool) string {
	t.Helper()
	dir := federationWorkspaceWithSolutionRegistration(t)
	path := filepath.Join(dir, manifest.FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hosted := `    - id: accounts
      protocol: connect
      module: host
      service: accounts
      endpoint: connect
      as: accounts
`
	documents := `    - id: documents
      protocol: connect
      module: documents
      service: api
      endpoint: connect
      as: documents
`
	replacement := documents + hosted
	if only {
		replacement = hosted
	}
	updated := []byte(replaceOnce(t, string(data), documents, replacement))
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func replaceOnce(t *testing.T, s, old, replacement string) string {
	t.Helper()
	index := -1
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			if index >= 0 {
				t.Fatalf("%q occurs twice", old)
			}
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("%q not found", old)
	}
	return s[:index] + replacement + s[index+len(old):]
}

// A prefix bound to the registrar's own module is the host's surface, routed by
// the host's catalog: the deployed store must hold neither a registration secret
// the solution would register a dead route with, nor an identity credential
// whose plaintext no service ever receives (the registrar's module is handed
// nothing). Only the federated module's credentials are derived.
func TestDeployedFederationSecretsDeriveNothingForTheHostsOwnPrefix(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, federationWorkspaceConsumingTheHost(t, false))

	secrets, err := DeployedFederationSecrets(ctx, workspace)
	if err != nil {
		t.Fatalf("DeployedFederationSecrets: %v", err)
	}
	for _, credential := range secrets.Credentials() {
		if credential.Identity == "accounts" {
			t.Errorf("derived %s for the registrar's own prefix", credential)
		}
	}
	registration := Credential{Kind: ModuleRegistration, Identity: "documents"}
	identity := Credential{Kind: ModuleIdentity, Identity: "documents"}
	if got, _ := secrets.For("wiki/backend", moduleRegistrationSecretsEnvironmentVariable); !slices.Equal(got.Credentials, []Credential{registration}) {
		t.Errorf("wiki registration map derives from %v, want only documents", got.Credentials)
	}
	if got, _ := secrets.For("host/accounts", workspaceSecretKey(federationConfigurationGroup, moduleIdentitySecretsKey)); !slices.Equal(got.Credentials, []Credential{identity}) {
		t.Errorf("registrar identity digests derive from %v, want only documents: every identity digest must have a service holding its plaintext", got.Credentials)
	}
	// Every identity digest the registrar holds has a holder.
	digests, _ := secrets.For("host/accounts", workspaceSecretKey(federationConfigurationGroup, moduleIdentitySecretsKey))
	for _, credential := range digests.Credentials {
		held := false
		for unique := range secrets.Services {
			if derivation, ok := secrets.For(unique, moduleIdentitySecretEnvironmentVariable); ok && slices.Contains(derivation.Credentials, credential) {
				held = true
			}
		}
		if !held {
			t.Errorf("registrar holds the digest of %s, which no service holds", credential)
		}
	}
}

// A solution consuming only the host renders no reference to registration
// secrets at all — the store would otherwise have to hold a key the derivation
// never produces — and still gets its projection.
func TestDerivedDeployInputsReferencesNoRegistrationForAHostOnlyConsumer(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, federationWorkspaceConsumingTheHost(t, true))

	derived, err := DerivedDeployInputs(ctx, workspace)
	if err != nil {
		t.Fatalf("DerivedDeployInputs: %v", err)
	}
	backend := derived.For("wiki/backend")
	if backend.Public[manifest.APIConsumesEnvironmentVariable] == "" {
		t.Error("the backend lost its api.consumes projection")
	}
	if _, referenced := backend.Secrets[moduleRegistrationSecretsEnvironmentVariable]; referenced {
		t.Errorf("backend references %s though the host routes its only consumed API itself", moduleRegistrationSecretsEnvironmentVariable)
	}
	secrets, err := DeployedFederationSecrets(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, derived := secrets.For("wiki/backend", moduleRegistrationSecretsEnvironmentVariable); derived {
		t.Error("deploy secrets derives a registration map the render does not reference")
	}
}

// The run follows the same rule, so a local run and a deployed cell federate the
// same prefixes: nothing is provisioned for the host's own accounts API.
func TestDerivedRunInputsProvisionsNothingForTheHostsOwnPrefix(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, federationWorkspaceConsumingTheHost(t, false))

	derived, err := DerivedRunInputs(ctx, workspace, wikiModuleIn(workspace), wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	secrets := parsePairs(t, derived.Overrides["wiki/backend"][moduleRegistrationSecretsEnvironmentVariable])
	if _, present := secrets["accounts"]; present || secrets["documents"] == "" {
		t.Errorf("backend registration secrets cover %v, want documents only", secrets)
	}
	declared := derived.WorkspaceConfigurations[federationConfigurationGroup]
	for _, key := range []string{moduleRegistrationSecretsKey, moduleIdentitySecretsKey} {
		if _, present := parsePairs(t, declared[key])["accounts"]; present {
			t.Errorf("registrar declares an accounts digest under %s", key)
		}
	}
}
