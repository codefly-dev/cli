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
