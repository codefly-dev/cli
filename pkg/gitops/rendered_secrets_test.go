package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
		!reflect.DeepEqual(secret.Services, []string{"billing/api"}) || !reflect.DeepEqual(secret.Properties, []string{"A_KEY", "B_KEY"}) {
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
	writeManagedProjection(t, workspace, "payments", "staging", "workos", "workos-credentials", []string{"WORKOS_API_KEY"})

	rendered, err := RenderedServiceSecrets(workspace, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Secrets) != 1 {
		t.Fatalf("secrets = %+v, want the managed service's remote key", rendered.Secrets)
	}
	secret := rendered.Secrets[0]
	if secret.RemoteKey != "workos-credentials" || !reflect.DeepEqual(secret.Services, []string{"payments/workos"}) ||
		!reflect.DeepEqual(secret.Properties, []string{"WORKOS_API_KEY"}) {
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
