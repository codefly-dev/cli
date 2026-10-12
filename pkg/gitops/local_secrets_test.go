package gitops

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

const localSecretValue = "s3cr3t-value-for-test"

// A local environment's referenced secret values are written for the local run
// to apply, privately, outside the rendered tree.
func TestWriteLocalSecretsWritesAPrivateSecretPerService(t *testing.T) {
	workspace := t.TempDir()
	keys := map[string][]string{"saas/accounts": {"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__TOKEN"}}
	values := map[string]map[string]string{"saas/accounts": {"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__TOKEN": localSecretValue}}
	if err := writeLocalSecrets(workspace, "local", "saas", "platform-saas", keys, values, nil, nil); err != nil {
		t.Fatal(err)
	}
	dir := LocalSecretsDir(workspace, "local", "saas")
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("local secrets dir mode %v (%v), want 0700", info.Mode().Perm(), err)
	}
	file := filepath.Join(dir, "accounts.json")
	info, err = os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("local secret file mode %v (%v), want 0600", info.Mode().Perm(), err)
	}
	manifests, err := LocalSecretManifests(workspace, "local", "saas")
	if err != nil || len(manifests) != 1 {
		t.Fatalf("manifests %d (%v), want 1", len(manifests), err)
	}
	var secret struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		StringData map[string]string `json:"stringData"`
	}
	if err = json.Unmarshal(manifests[0], &secret); err != nil {
		t.Fatal(err)
	}
	if secret.Kind != "Secret" || secret.Metadata.Name != "secret-accounts" || secret.Metadata.Namespace != "platform-saas" ||
		secret.StringData["CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__TOKEN"] != localSecretValue {
		t.Fatalf("unexpected local secret: %+v", secret)
	}
}

// A referenced key with no value refuses, naming the key and never a value.
func TestWriteLocalSecretsRefusesAReferenceWithNoValue(t *testing.T) {
	keys := map[string][]string{"saas/accounts": {"PRESENT", "ABSENT"}}
	values := map[string]map[string]string{"saas/accounts": {"PRESENT": localSecretValue}}
	err := writeLocalSecrets(t.TempDir(), "local", "saas", "ns", keys, values, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "saas/accounts ABSENT") || strings.Contains(err.Error(), localSecretValue) {
		t.Fatalf("a missing value must refuse by key and print no value: %v", err)
	}
}

// Under git, secrets are written only where git ignores them.
func TestWriteLocalSecretsRefusesAWorkspaceThatWouldCommitThem(t *testing.T) {
	workspace := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", workspace).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	keys := map[string][]string{"saas/accounts": {"K"}}
	values := map[string]map[string]string{"saas/accounts": {"K": localSecretValue}}
	if err := writeLocalSecrets(workspace, "local", "saas", "ns", keys, values, nil, nil); err == nil || !strings.Contains(err.Error(), ".gitignore") {
		t.Fatalf("an unignored .codefly must refuse: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte(".codefly/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeLocalSecrets(workspace, "local", "saas", "ns", keys, values, nil, nil); err != nil {
		t.Fatalf("an ignored .codefly must be written: %v", err)
	}
}

// A local secrets directory others can read is refused at apply time.
func TestLocalSecretManifestsRefusesAnOpenDirectory(t *testing.T) {
	workspace := t.TempDir()
	dir := LocalSecretsDir(workspace, "local", "saas")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LocalSecretManifests(workspace, "local", "saas"); err == nil {
		t.Fatal("an open local secrets directory was read")
	}
}

// A consumer's templated secret (a connection string) is assembled locally
// from its producer's own primitives, as a cell's ExternalSecret assembles it
// from the store.
func TestWriteLocalSecretsAssemblesAProducersTemplate(t *testing.T) {
	const password = "p@ss word"
	producer := "saas/store"
	primitive := resources.ServiceSecretConfigurationKeyFromUnique(producer, "postgres", "POSTGRES_PASSWORD")
	consumerKey := resources.ServiceSecretConfigurationKeyFromUnique(producer, "postgres", "CONNECTION")
	template := &basev0.ConfigurationValueTemplate{Segments: []*basev0.ConfigurationValueTemplateSegment{
		{Content: &basev0.ConfigurationValueTemplateSegment_Literal{Literal: "postgresql://user:"}},
		{Content: &basev0.ConfigurationValueTemplateSegment_Reference{Reference: &basev0.ConfigurationValueReference{
			Configuration: "postgres", Key: "POSTGRES_PASSWORD", Escape: basev0.ConfigurationValueEscape_CONFIGURATION_VALUE_ESCAPE_URL_USERINFO,
		}}},
		{Content: &basev0.ConfigurationValueTemplateSegment_Literal{Literal: "@store:5432/users"}},
	}}
	templates := renderTemplates{consumerKey: {producer: producer, template: template}}
	keys := map[string][]string{"saas/accounts": {consumerKey}, producer: {primitive}}
	values := map[string]map[string]string{producer: {primitive: password}}
	workspace := t.TempDir()
	if err := writeLocalSecrets(workspace, "local", "saas", "ns", keys, values, templates, nil); err != nil {
		t.Fatal(err)
	}
	manifests, err := LocalSecretManifests(workspace, "local", "saas")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, manifest := range manifests {
		var secret localSecretManifest
		if err = json.Unmarshal(manifest, &secret); err != nil {
			t.Fatal(err)
		}
		if got, ok := secret.StringData[consumerKey]; ok {
			found = true
			if !strings.HasPrefix(got, "postgresql://user:p%40ss") || !strings.HasSuffix(got, "@store:5432/users") {
				t.Fatalf("assembled connection = %q", got)
			}
		}
	}
	if !found {
		t.Fatal("the consumer's templated secret was not assembled")
	}
}
