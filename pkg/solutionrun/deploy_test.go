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
	"github.com/codefly-dev/core/solution/manifest"
)

// A render derives the run's carriers under the run's names, but never a secret
// value: the public projection and prefix are values, every credential is a
// reference to the key the environment's secret store holds it under.
func TestDerivedDeployInputsProjectsBothEndsWithoutAnySecretValue(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")

	derived, err := DerivedDeployInputs(ctx, workspace)
	if err != nil {
		t.Fatalf("DerivedDeployInputs: %v", err)
	}

	backend := derived.For("wiki/backend")
	solutionManifest, err := moduleManifest(wikiModuleIn(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.Public[manifest.APIConsumesEnvironmentVariable]; got != solutionManifest.ConsumedAPIsEnvValue() || got == "" {
		t.Errorf("backend %s = %q, want the run's projection %q", manifest.APIConsumesEnvironmentVariable, got, solutionManifest.ConsumedAPIsEnvValue())
	}
	if !reflect.DeepEqual(backend.Secrets, map[string]string{moduleRegistrationSecretsEnvironmentVariable: moduleRegistrationSecretsEnvironmentVariable}) {
		t.Errorf("backend secrets = %v, want only a reference to its registration secrets", backend.Secrets)
	}

	for _, unique := range []string{"documents/api", "documents/worker"} {
		injection := derived.For(unique)
		if !reflect.DeepEqual(injection.Public, map[string]string{moduleIdentityPrefixEnvironmentVariable: "documents"}) {
			t.Errorf("%s public = %v, want only its identity prefix", unique, injection.Public)
		}
		// The deprecated alias resolves to the identity secret — one stored value
		// — and never to the registration secret the backend presents.
		want := map[string]string{
			moduleIdentitySecretEnvironmentVariable:     moduleIdentitySecretEnvironmentVariable,
			moduleRegistrationSecretEnvironmentVariable: moduleIdentitySecretEnvironmentVariable,
		}
		if !reflect.DeepEqual(injection.Secrets, want) {
			t.Errorf("%s secrets = %v, want %v", unique, injection.Secrets, want)
		}
	}

	// The registrar holds the digests and is handed nothing.
	for _, unique := range []string{"host/accounts", "host/gateway"} {
		if injection, exists := derived.Services[unique]; exists {
			t.Errorf("registrar module service %s received %+v", unique, injection)
		}
	}
	// No value may be a minted secret: every public value is a projection or a
	// prefix, and a secret carrier maps to a store key, never to material.
	for unique, injection := range derived.Services {
		for key, storeKey := range injection.Secrets {
			if !strings.HasPrefix(storeKey, "CODEFLY__") {
				t.Errorf("%s %s resolves to %q, which is not a store key", unique, key, storeKey)
			}
		}
		for key := range injection.Public {
			if resources.IsSensitiveKey(key) {
				t.Errorf("%s renders the credential-named key %s as a value", unique, key)
			}
		}
	}
}

// Without a registrar nothing can admit a secret, so a render withholds every
// reference — exactly as a run withholds every secret — keeps the projection,
// and says so.
func TestDerivedDeployInputsWithholdsSecretsWithoutARegistrar(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "host" })

	derived, err := DerivedDeployInputs(ctx, workspace)
	if err != nil {
		t.Fatalf("DerivedDeployInputs: %v", err)
	}
	for unique, injection := range derived.Services {
		if len(injection.Secrets) > 0 {
			t.Errorf("%s received secret references %v with no registrar", unique, injection.Secrets)
		}
	}
	if derived.For("wiki/backend").Public[manifest.APIConsumesEnvironmentVariable] == "" {
		t.Error("withholding the secrets also dropped the api.consumes projection")
	}
	if !slices.ContainsFunc(derived.Notes, func(note Note) bool {
		return note.Warning && strings.Contains(note.Message, federationConfigurationGroup)
	}) {
		t.Errorf("withholding was not reported as a warning: %+v", derived.Notes)
	}
}

// A workspace composing no solution renders exactly as before.
func TestDerivedDeployInputsNoOpsWithoutASolution(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "wiki" })

	derived, err := DerivedDeployInputs(ctx, workspace)
	if err != nil {
		t.Fatalf("DerivedDeployInputs: %v", err)
	}
	if len(derived.Services) != 0 {
		t.Fatalf("derived %+v for a workspace with no solution", derived.Services)
	}
}

// One module, one identity: two solutions binding the same module under
// different prefixes would need two identity carriers on one set of services,
// so the render refuses rather than picking whichever it saw first.
func TestDerivedDeployInputsRefusesAModuleBoundUnderTwoPrefixes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/solution-federation")); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(dir, "modules", "notes")
	for path, content := range map[string]string{
		filepath.Join(notes, "module.codefly.yaml"): "kind: module\nname: notes\nservice-entry: backend\nservices:\n    - name: backend\n",
		filepath.Join(notes, "services", "backend", "service.codefly.yaml"): "kind: service\nname: backend\nversion: 0.0.0\nmodule: notes\n" +
			"agent:\n    publisher: codefly.dev\n    kind: codefly:service\n    name: go\n    version: 0.0.1\n",
		filepath.Join(notes, manifest.FileName): strings.Replace(
			strings.Replace(solutionManifestWithConsumes, "name: wiki", "name: notes", 1),
			"as: documents", "as: knowledge", 1),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspacePath := filepath.Join(dir, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspacePath, append(data, []byte("    - name: notes\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := loadTestWorkspace(t, dir)

	_, err = DerivedDeployInputs(ctx, workspace)
	if err == nil || !strings.Contains(err.Error(), "one prefix") {
		t.Fatalf("DerivedDeployInputs = %v, want a refusal naming both prefixes", err)
	}
	for _, want := range []string{`"documents"`, `"knowledge"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name the prefix %s", err, want)
		}
	}
}
