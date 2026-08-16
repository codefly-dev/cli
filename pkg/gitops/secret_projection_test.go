package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func workosSecretReferences() []resources.EnvironmentManagedSecretReference {
	store := resources.EnvironmentSecretStoreReference{Name: "azure-keyvault-prod", Kind: "ClusterSecretStore"}
	return []resources.EnvironmentManagedSecretReference{
		{Name: "client-id", RemoteKey: "workos/client-id", SecretStore: store},
		{Name: "api-key", RemoteKey: "workos/api-key", SecretStore: store},
	}
}

func TestManagedSecretProjectionRendersExternalSecret(t *testing.T) {
	projection, err := managedSecretProjection("workos", "payments", workosSecretReferences())
	if err != nil {
		t.Fatal(err)
	}
	if projection.APIVersion != externalSecretAPIVersion || projection.Kind != kindExternalSecret {
		t.Fatalf("projection identity = %v/%v", projection.APIVersion, projection.Kind)
	}
	if projection.Metadata.Name != "secret-workos" || projection.Metadata.Namespace != "payments" {
		t.Fatalf("projection metadata = %+v", projection.Metadata)
	}
	if projection.Spec.SecretStoreRef.Name != "azure-keyvault-prod" || projection.Spec.SecretStoreRef.Kind != "ClusterSecretStore" {
		t.Fatalf("projection store = %+v", projection.Spec.SecretStoreRef)
	}
	if projection.Spec.Target.Name != "secret-workos" {
		t.Fatalf("projection target = %+v", projection.Spec.Target)
	}
	if len(projection.Spec.Data) != 2 {
		t.Fatalf("projection data length = %d", len(projection.Spec.Data))
	}
	if projection.Spec.Data[0].SecretKey != "client-id" || projection.Spec.Data[0].RemoteRef.Key != "workos/client-id" {
		t.Fatalf("projection first entry = %+v", projection.Spec.Data[0])
	}
}

func TestManagedSecretProjectionPassesPromotableValidation(t *testing.T) {
	projection, err := managedSecretProjection("workos", "payments", workosSecretReferences())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	manifests, _, err := decodeYAML("external-secret.yaml", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("projection decoded into %d manifests", len(manifests))
	}
	if err := validateManifest(manifests[0], nil, true); err != nil {
		t.Fatalf("rendered projection is not promotable: %v", err)
	}
}

func TestManagedSecretProjectionEmptyReferencesRenderNothing(t *testing.T) {
	projection, err := managedSecretProjection("workos", "payments", nil)
	if err != nil {
		t.Fatal(err)
	}
	if projection != nil {
		t.Fatalf("empty references rendered a projection: %v", projection)
	}
}

func TestManagedSecretProjectionRejectsInvalidDeclarations(t *testing.T) {
	store := resources.EnvironmentSecretStoreReference{Name: "azure-keyvault-prod", Kind: "ClusterSecretStore"}
	tests := []struct {
		name      string
		namespace string
		refs      []resources.EnvironmentManagedSecretReference
	}{
		{
			name:      "missing namespace",
			namespace: "",
			refs:      []resources.EnvironmentManagedSecretReference{{Name: "api-key", RemoteKey: "workos/api-key", SecretStore: store}},
		},
		{
			name:      "missing remote key",
			namespace: "payments",
			refs:      []resources.EnvironmentManagedSecretReference{{Name: "api-key", SecretStore: store}},
		},
		{
			name:      "incomplete store",
			namespace: "payments",
			refs:      []resources.EnvironmentManagedSecretReference{{Name: "api-key", RemoteKey: "workos/api-key", SecretStore: resources.EnvironmentSecretStoreReference{Name: "azure-keyvault-prod"}}},
		},
		{
			name:      "diverging stores",
			namespace: "payments",
			refs: []resources.EnvironmentManagedSecretReference{
				{Name: "api-key", RemoteKey: "workos/api-key", SecretStore: store},
				{Name: "client-id", RemoteKey: "workos/client-id", SecretStore: resources.EnvironmentSecretStoreReference{Name: "other", Kind: "SecretStore"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := managedSecretProjection("workos", test.namespace, test.refs); err == nil {
				t.Fatalf("expected error for %s", test.name)
			}
		})
	}
}

func TestRetainManagedBootstrapProjectsSecretsWithoutBootstrapJobs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	retained, err := retainManagedBootstrap(root, "workos", "production", "payments", workosSecretReferences())
	if err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("managed service with secret references was not retained")
	}
	baseKustomization, err := os.ReadFile(filepath.Join(root, "base", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(baseKustomization), "external-secret.yaml") {
		t.Fatalf("base kustomization does not reference the projection: %s", baseKustomization)
	}

	overlay := filepath.Join(root, "overlays", "production")
	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rendered, err := kustomizer.Run(filesys.MakeFsOnDisk(), overlay)
	if err != nil {
		t.Fatalf("build managed overlay: %v", err)
	}
	encoded, err := rendered.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	manifests, _, err := decodeYAML("overlay.yaml", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || manifests[0].kind != "ExternalSecret" {
		t.Fatalf("managed overlay rendered %d manifests, want one ExternalSecret", len(manifests))
	}
	if err := validateManifest(manifests[0], nil, true); err != nil {
		t.Fatalf("managed overlay projection is not promotable: %v", err)
	}
}

func TestRetainManagedBootstrapRemovesTreeWithoutJobsOrSecrets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retained, err := retainManagedBootstrap(root, "cache", "production", "payments", nil)
	if err != nil {
		t.Fatal(err)
	}
	if retained {
		t.Fatal("managed service without jobs or secrets was retained")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("managed tree survived: %v", err)
	}
}
