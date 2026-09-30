package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const renderedExternalSecret = `apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
    name: secret-%s
    namespace: example-%s
spec:
    refreshInterval: 1h
    secretStoreRef:
        name: cell-secrets
        kind: ClusterSecretStore
    target:
        name: secret-%s
    data:
%s`

// writeRenderedModule lays down a module render for environment: an inventory
// recording each service's projected ExternalSecret with its digest.
func writeRenderedModule(t *testing.T, workspace, module, environment string, services map[string][]string) string {
	t.Helper()
	root := filepath.Join(workspace, "deployments", "modules", module)
	inventory := Inventory{SchemaVersion: SchemaVersion, Module: module, Environment: environment, AppProject: "example", OwnedPath: "deployments/modules/" + module}
	for service, properties := range services {
		var data strings.Builder
		for _, property := range properties {
			data.WriteString("        - secretKey: " + property + "\n          remoteRef:\n            key: example-" + module + "-" + service + "\n            property: " + property + "\n")
		}
		content := []byte(fmt.Sprintf(renderedExternalSecret, service, module, service, data.String()))
		relative := "services/" + service + "/overlays/" + environment + "/external-secret.yaml"
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		inventory.Units = append(inventory.Units, InventoryUnit{Kind: "service", Module: module, Name: service, Path: "services/" + service})
		inventory.Files = append(inventory.Files, InventoryFile{Path: relative, SHA256: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(content))})
	}
	encoded, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, InventoryFilename), append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRenderedServiceSecretsReadsTheEnvironmentsProjections(t *testing.T) {
	workspace := t.TempDir()
	writeRenderedModule(t, workspace, "billing", "staging", map[string][]string{"api": {"B_KEY", "A_KEY"}})
	writeRenderedModule(t, workspace, "search", "production", map[string][]string{"api": {"C_KEY"}})
	// A render's staging directory is never a module.
	if err := os.MkdirAll(filepath.Join(workspace, "deployments", "modules", ".codefly-render-123"), 0o755); err != nil {
		t.Fatal(err)
	}

	rendered, err := RenderedServiceSecrets(workspace, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rendered.Modules, []string{"billing"}) || !reflect.DeepEqual(rendered.Skipped, []string{"search"}) {
		t.Fatalf("modules %v skipped %v", rendered.Modules, rendered.Skipped)
	}
	if len(rendered.Secrets) != 1 {
		t.Fatalf("secrets = %+v", rendered.Secrets)
	}
	secret := rendered.Secrets[0]
	if secret.RemoteKey != "example-billing-api" || secret.Store.Name != "cell-secrets" || secret.Namespace != "example-billing" ||
		!reflect.DeepEqual(secret.Services, []string{"billing/api"}) || !reflect.DeepEqual(secret.Properties, []RenderedSecretProperty{{Property: "A_KEY", Keys: []string{"A_KEY"}, Readers: []RenderedSecretReader{{Service: "billing/api", Key: "A_KEY"}}}, {Property: "B_KEY", Keys: []string{"B_KEY"}, Readers: []RenderedSecretReader{{Service: "billing/api", Key: "B_KEY"}}}}) {
		t.Errorf("secret = %+v", secret)
	}
}

// An ExternalSecret edited after the render names keys no render derived; the
// store is never seeded from it.
func TestRenderedServiceSecretsRefusesAnEditedProjection(t *testing.T) {
	workspace := t.TempDir()
	root := writeRenderedModule(t, workspace, "billing", "staging", map[string][]string{"api": {"A_KEY"}})
	path := filepath.Join(root, "services", "api", "overlays", "staging", "external-secret.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "A_KEY", "EDITED_KEY")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderedServiceSecrets(workspace, "staging"); err == nil || !strings.Contains(err.Error(), "re-render") {
		t.Fatalf("RenderedServiceSecrets = %v, want a refusal", err)
	}
}

// writeManagedProjection lays down a managed service's render as
// retainManagedBundle leaves it: the ExternalSecret is the bundle's base, and
// the environment overlay only includes it.
func writeManagedProjection(t *testing.T, workspace, module, environment, service, remoteKey string, properties []string) string {
	t.Helper()
	root := filepath.Join(workspace, "deployments", "modules", module)
	var data strings.Builder
	for _, property := range properties {
		data.WriteString("        - secretKey: " + property + "\n          remoteRef:\n            key: " + remoteKey + "\n            property: " + property + "\n")
	}
	content := []byte(fmt.Sprintf(renderedExternalSecret, service, module, service, data.String()))
	relative := "services/" + service + "/base/external-secret.yaml"
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	inventory := Inventory{SchemaVersion: SchemaVersion, Module: module, Environment: environment, AppProject: "example", OwnedPath: "deployments/modules/" + module}
	inventory.Units = append(inventory.Units, InventoryUnit{Kind: "service", Module: module, Name: service,
		Path: "services/" + service, Managed: true, Bootstrap: true})
	inventory.Files = append(inventory.Files, InventoryFile{Path: relative, SHA256: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(content))})
	encoded, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, InventoryFilename), append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// A managed service has no overlay of its own: retainManagedBundle assembles the
// bundle from its bootstrap Jobs and its projection, and the projection is the
// base every environment's overlay includes. Looking only in the overlay left
// every remote key an environment's managed-services secret-references name out
// of the plan, so the keys an operator was told to supply omitted exactly the
// external credentials the environment enumerates.
func TestRenderedServiceSecretsReadsAManagedServicesProjection(t *testing.T) {
	workspace := t.TempDir()
	writeManagedProjection(t, workspace, "payments", "staging", "identity", "identity-credentials", []string{"API_KEY"})

	rendered, err := RenderedServiceSecrets(workspace, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Secrets) != 1 {
		t.Fatalf("secrets = %+v, want the managed service's remote key", rendered.Secrets)
	}
	secret := rendered.Secrets[0]
	if secret.RemoteKey != "identity-credentials" || !reflect.DeepEqual(secret.Services, []string{"payments/identity"}) ||
		!reflect.DeepEqual(secret.Properties, []RenderedSecretProperty{{Property: "API_KEY", Keys: []string{"API_KEY"}, Readers: []RenderedSecretReader{{Service: secret.Services[0], Key: "API_KEY"}}}}) {
		t.Errorf("secret = %+v", secret)
	}
}

// Deleting a projection the render recorded takes its keys out of the plan
// silently, which is worse than editing it: a federation credential whose only
// carrier left that way is one the registrar is then handed a digest of and no
// service the plaintext. Both are the same tampering and both are refused.
func TestRenderedServiceSecretsRefusesADeletedProjection(t *testing.T) {
	workspace := t.TempDir()
	root := writeRenderedModule(t, workspace, "billing", "staging", map[string][]string{"api": {"A_KEY"}})
	if err := os.Remove(filepath.Join(root, "services", "api", "overlays", "staging", "external-secret.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderedServiceSecrets(workspace, "staging"); err == nil || !strings.Contains(err.Error(), "re-render") {
		t.Fatalf("RenderedServiceSecrets = %v, want a refusal", err)
	}
}

// A SecretStore is namespaced, so the same name in two namespaces is two objects
// resolving through two backends. Grouping by remote key alone collapses them and
// leaves whichever namespace was read first deciding the backend for both.
func TestRenderedServiceSecretsRefusesOneKeyThroughTwoNamespacedStores(t *testing.T) {
	workspace := t.TempDir()
	for _, module := range []string{"billing", "search"} {
		root := filepath.Join(workspace, "deployments", "modules", module)
		content := []byte(strings.ReplaceAll(
			fmt.Sprintf(renderedExternalSecret, "api", module, "api",
				"        - secretKey: A_KEY\n          remoteRef:\n            key: shared\n            property: A_KEY\n"),
			"kind: ClusterSecretStore", "kind: SecretStore"))
		relative := "services/api/overlays/staging/external-secret.yaml"
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		inventory := Inventory{SchemaVersion: SchemaVersion, Module: module, Environment: "staging", AppProject: "example", OwnedPath: "deployments/modules/" + module}
		inventory.Units = []InventoryUnit{{Kind: "service", Module: module, Name: "api", Path: "services/api"}}
		inventory.Files = []InventoryFile{{Path: relative, SHA256: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(content))}}
		encoded, err := json.MarshalIndent(inventory, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, InventoryFilename), append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := RenderedServiceSecrets(workspace, "staging")
	if err == nil || !strings.Contains(err.Error(), "two different stores") {
		t.Fatalf("RenderedServiceSecrets = %v, want a refusal naming the two namespaces", err)
	}
}

// writeMappedRender lays down one service's render where the environment files
// each secret key under a store property of its own naming — what
// `service-secrets.services.<svc>.remote-keys` produces. The key and the
// property are then different strings for every entry.
func writeMappedRender(t *testing.T, workspace, module, service, environment, remoteKey string, keysByProperty map[string][]string) {
	t.Helper()
	root := filepath.Join(workspace, "deployments", "modules", module)
	var data strings.Builder
	properties := make([]string, 0, len(keysByProperty))
	for property := range keysByProperty {
		properties = append(properties, property)
	}
	sort.Strings(properties)
	for _, property := range properties {
		for _, key := range keysByProperty[property] {
			data.WriteString("        - secretKey: " + key + "\n          remoteRef:\n            key: " + remoteKey + "\n            property: " + property + "\n")
		}
	}
	content := []byte(fmt.Sprintf(renderedExternalSecret, service, module, service, data.String()))
	relative := "services/" + service + "/overlays/" + environment + "/external-secret.yaml"
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	inventory := Inventory{SchemaVersion: SchemaVersion, Module: module, Environment: environment, AppProject: "example", OwnedPath: "deployments/modules/" + module}
	inventory.Units = append(inventory.Units, InventoryUnit{Kind: "service", Module: module, Name: service, Path: "services/" + service})
	inventory.Files = append(inventory.Files, InventoryFile{Path: relative, SHA256: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(content))})
	encoded, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, InventoryFilename), append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The property is where a value is filed; the secretKey is what it is. An
// environment that maps them apart must still report both, or everything that
// resolves a value by its key — a federation derivation, a declared generator, a
// configuration value shared across remote keys — recognizes none of them.
func TestRenderedServiceSecretsRecordsTheKeyReadFromEachProperty(t *testing.T) {
	workspace := t.TempDir()
	writeMappedRender(t, workspace, "saas", "store", "staging", "cell-store", map[string][]string{
		"postgres_user":       {"CODEFLY__SERVICE_SECRET_CONFIGURATION__SAAS__STORE__POSTGRES__POSTGRES_USER"},
		"read_write_password": {"CODEFLY__SERVICE_SECRET_CONFIGURATION__SAAS__STORE__POSTGRES__POSTGRES_READ_WRITE_PASSWORD"},
	})

	rendered, err := RenderedServiceSecrets(workspace, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Secrets) != 1 {
		t.Fatalf("secrets = %+v", rendered.Secrets)
	}
	want := []RenderedSecretProperty{
		{Property: "postgres_user", Keys: []string{"CODEFLY__SERVICE_SECRET_CONFIGURATION__SAAS__STORE__POSTGRES__POSTGRES_USER"}},
		{Property: "read_write_password", Keys: []string{"CODEFLY__SERVICE_SECRET_CONFIGURATION__SAAS__STORE__POSTGRES__POSTGRES_READ_WRITE_PASSWORD"}},
	}
	for i := range want {
		for _, key := range want[i].Keys {
			want[i].Readers = append(want[i].Readers, RenderedSecretReader{Service: rendered.Secrets[0].Services[0], Key: key})
		}
	}
	if !reflect.DeepEqual(rendered.Secrets[0].Properties, want) {
		t.Errorf("properties = %+v", rendered.Secrets[0].Properties)
	}
}

// Two services reading one property under one key is one entry; the key is not
// repeated.
func TestRenderedServiceSecretsDeduplicatesTheKeysOfOneProperty(t *testing.T) {
	workspace := t.TempDir()
	writeMappedRender(t, workspace, "saas", "accounts", "staging", "cell-auth", map[string][]string{
		"internal_token": {"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__TOKEN", "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__TOKEN"},
	})

	rendered, err := RenderedServiceSecrets(workspace, "staging")
	if err != nil {
		t.Fatal(err)
	}
	want := []RenderedSecretProperty{{Property: "internal_token", Keys: []string{"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__TOKEN"}}}
	for i := range want {
		for _, key := range want[i].Keys {
			want[i].Readers = append(want[i].Readers, RenderedSecretReader{Service: rendered.Secrets[0].Services[0], Key: key})
		}
	}
	if !reflect.DeepEqual(rendered.Secrets[0].Properties, want) {
		t.Errorf("properties = %+v", rendered.Secrets[0].Properties)
	}
}

// An entry with no secretKey names where a value lives and never what it is.
// Reading the property as the key would report a derivable value as one to type.
func TestRenderedServiceSecretsRefusesAnEntryWithNoSecretKey(t *testing.T) {
	workspace := t.TempDir()
	writeMappedRender(t, workspace, "saas", "store", "staging", "cell-store", map[string][]string{"postgres_user": {""}})

	_, err := RenderedServiceSecrets(workspace, "staging")
	if err == nil || !strings.Contains(err.Error(), "into no secret key") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderedSecretsKeepActualServicePropertyBindings(t *testing.T) {
	workspace := t.TempDir()
	key := "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__SOLUTION_REGISTRATION__SECRET"
	writeMappedRender(t, workspace, "first", "api", "staging", "shared", map[string][]string{"first_registration": {key}})
	writeMappedRender(t, workspace, "second", "api", "staging", "shared", map[string][]string{"second_registration": {key}})
	rendered, err := RenderedServiceSecrets(workspace, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Secrets) != 1 || len(rendered.Secrets[0].Properties) != 2 {
		t.Fatal("shared document properties were lost")
	}
	for i, service := range []string{"first/api", "second/api"} {
		readers := rendered.Secrets[0].Properties[i].Readers
		if !reflect.DeepEqual(readers, []RenderedSecretReader{{Service: service, Key: key}}) {
			t.Fatal("service/key association was lost")
		}
	}
}
