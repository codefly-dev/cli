package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

func TestSecretsInputBindsEveryStoreAndRejectsPayloadFields(t *testing.T) {
	document := secretsInputDocument{Schema: secretsInputSchema, Environment: "azure-staging",
		Rendered: gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
			{Store: environments.EnvironmentSecretStoreReference{Kind: "ClusterSecretStore", Name: "cell-secrets"}},
		}}}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretsInput(bytes.NewReader(raw), "cell-secrets"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "another-store"} {
		if _, err := readSecretsInput(bytes.NewReader(raw), name); err == nil {
			t.Fatal("accepted wrong store")
		}
	}
	for _, changed := range []string{
		strings.Replace(string(raw), `"Schema":`, `"Values":{"token":"never-print"},"Schema":`, 1),
		string(raw) + `{}`, strings.Replace(string(raw), secretsInputSchema, "unknown/v2", 1),
		strings.Replace(string(raw), "ClusterSecretStore", "SecretStore", 1),
	} {
		if _, err := readSecretsInput(strings.NewReader(changed), "cell-secrets"); err == nil || strings.Contains(err.Error(), "never-print") {
			t.Fatal("invalid input was accepted or payload leaked")
		}
	}
}

func TestSuppliedValuesAreOneObjectAndNeverQuoted(t *testing.T) {
	key := "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__IDENTITY__IDENTITY_CLIENT_SECRET"
	rendered := gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{{
		RemoteKey:  "host-accounts",
		Properties: []gitops.RenderedSecretProperty{{Property: key, Keys: []string{key}}},
	}}}
	path := t.TempDir() + "/supplied.json"
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"` + key + `":"never-print"}`)
	supplied, err := readSuppliedValues(path, rendered, nil)
	if err != nil || supplied[key] != "never-print" {
		t.Fatalf("readSuppliedValues = %v, %v", supplied, err)
	}
	for _, content := range []string{
		`["never-print"]`, `{"` + key + `":"never-print"}{}`, `{"` + key + `":1}`, `never-print`,
		`{"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__TYPO__X":"never-print"}`,
	} {
		write(content)
		if _, err := readSuppliedValues(path, rendered, nil); err == nil || strings.Contains(err.Error(), "never-print") {
			t.Fatalf("accepted %q or leaked it: %v", content, err)
		}
	}
	if supplied, err := readSuppliedValues("", rendered, nil); err != nil || supplied != nil {
		t.Fatalf("no file = %v, %v; want nothing", supplied, err)
	}
}
