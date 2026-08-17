package gitops

import (
	"context"
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
	if projection.Spec.RefreshInterval != secretRefreshInterval {
		t.Fatalf("projection refreshInterval = %q, want %q", projection.Spec.RefreshInterval, secretRefreshInterval)
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
			name:      "backend type mistaken for store kind",
			namespace: "payments",
			refs:      []resources.EnvironmentManagedSecretReference{{Name: "api-key", RemoteKey: "workos/api-key", SecretStore: resources.EnvironmentSecretStoreReference{Name: "azure-keyvault-prod", Kind: "azure-keyvault"}}},
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

func TestRetainManagedBundleProjectsSecretsWithoutBootstrapJobs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	retained, err := retainManagedBundle(root, "workos", "production", "payments", workosSecretReferences())
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

func TestRetainManagedBundleRemovesTreeWithoutJobsOrSecrets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retained, err := retainManagedBundle(root, "cache", "production", "payments", nil)
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

func azureServiceSecrets() *resources.EnvironmentServiceSecrets {
	return &resources.EnvironmentServiceSecrets{
		SecretStore: resources.EnvironmentSecretStoreReference{Name: "azure-keyvault-prod", Kind: "ClusterSecretStore"},
		Services: map[string]resources.EnvironmentServiceSecretMapping{
			"accounts": {RemoteKeys: map[string]string{"workos-client-secret": "workos/prod/client-secret"}},
		},
	}
}

func TestServiceSecretProjectionDefaultsAndOverridesRemoteKeys(t *testing.T) {
	projection, err := serviceSecretProjection(
		"accounts", "payments", azureServiceSecrets(),
		[]string{"workos-api-key", "workos-client-secret"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Metadata.Name != "secret-accounts" || projection.Metadata.Namespace != "payments" {
		t.Fatalf("projection metadata = %+v", projection.Metadata)
	}
	if projection.Spec.SecretStoreRef.Name != "azure-keyvault-prod" || projection.Spec.SecretStoreRef.Kind != "ClusterSecretStore" {
		t.Fatalf("projection store = %+v", projection.Spec.SecretStoreRef)
	}
	if len(projection.Spec.Data) != 2 {
		t.Fatalf("projection data length = %d", len(projection.Spec.Data))
	}
	// Keys arrive sorted; the un-overridden key falls back to "<service>/<key>".
	if projection.Spec.Data[0].SecretKey != "workos-api-key" || projection.Spec.Data[0].RemoteRef.Key != "accounts/workos-api-key" {
		t.Fatalf("defaulted entry = %+v", projection.Spec.Data[0])
	}
	if projection.Spec.Data[1].SecretKey != "workos-client-secret" || projection.Spec.Data[1].RemoteRef.Key != "workos/prod/client-secret" {
		t.Fatalf("overridden entry = %+v", projection.Spec.Data[1])
	}
}

func TestServiceSecretProjectionRendersNothingWithoutStoreOrKeys(t *testing.T) {
	if projection, err := serviceSecretProjection("accounts", "payments", nil, []string{"api-key"}); err != nil || projection != nil {
		t.Fatalf("nil store rendered %v (err %v)", projection, err)
	}
	if projection, err := serviceSecretProjection("accounts", "payments", azureServiceSecrets(), nil); err != nil || projection != nil {
		t.Fatalf("no keys rendered %v (err %v)", projection, err)
	}
}

func TestServiceSecretProjectionHonorsPerServiceStore(t *testing.T) {
	secrets := &resources.EnvironmentServiceSecrets{
		SecretStore: resources.EnvironmentSecretStoreReference{Name: "env-default", Kind: "ClusterSecretStore"},
		Services: map[string]resources.EnvironmentServiceSecretMapping{
			"accounts": {SecretStore: &resources.EnvironmentSecretStoreReference{Name: "accounts-vault", Kind: "SecretStore"}},
		},
	}
	projection, err := serviceSecretProjection("accounts", "payments", secrets, []string{"api-key"})
	if err != nil {
		t.Fatal(err)
	}
	if projection.Spec.SecretStoreRef.Name != "accounts-vault" || projection.Spec.SecretStoreRef.Kind != "SecretStore" {
		t.Fatalf("per-service store override not honored: %+v", projection.Spec.SecretStoreRef)
	}

	// A service without an override still resolves through the environment store.
	other, err := serviceSecretProjection("billing", "payments", secrets, []string{"api-key"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Spec.SecretStoreRef.Name != "env-default" {
		t.Fatalf("service without override should use env store: %+v", other.Spec.SecretStoreRef)
	}
}

func TestServiceSecretProjectionRejectsInvalidStore(t *testing.T) {
	backendKind := &resources.EnvironmentServiceSecrets{
		SecretStore: resources.EnvironmentSecretStoreReference{Name: "azure-keyvault-prod", Kind: "azure-keyvault"},
	}
	if _, err := serviceSecretProjection("accounts", "payments", backendKind, []string{"api-key"}); err == nil {
		t.Fatal("expected error for backend-type store kind")
	}
	valid := azureServiceSecrets()
	if _, err := serviceSecretProjection("accounts", "", valid, []string{"api-key"}); err == nil {
		t.Fatal("expected error for missing namespace")
	}
}

func writeServiceTree(t *testing.T, root, environment string) {
	t.Helper()
	base := filepath.Join(root, "base")
	overlay := filepath.Join(root, "overlays", environment)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		t.Fatal(err)
	}
	deployment := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: accounts
  namespace: payments
spec:
  template:
    spec:
      containers:
        - name: accounts
          image: registry.example.com/accounts@sha256:` + strings.Repeat("a", 64) + `
          env:
            - name: WORKOS_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: secret-accounts
                  key: workos-client-secret
            - name: WORKOS_API_KEY
              valueFrom:
                secretKeyRef:
                  name: secret-accounts
                  key: workos-api-key
`
	if err := os.WriteFile(filepath.Join(base, "deployment.yaml"), []byte(deployment), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestServiceSecretKeysDiscoversReferencedKeys(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeServiceTree(t, root, "production")
	keys, err := serviceSecretKeys(root, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "workos-api-key" || keys[1] != "workos-client-secret" {
		t.Fatalf("discovered keys = %v", keys)
	}
}

func TestProjectServiceSecretsInjectsPromotableOverlay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeServiceTree(t, root, "production")
	projected, err := projectServiceSecrets(root, "accounts", "production", "payments", azureServiceSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if !projected {
		t.Fatal("service referencing secret-accounts was not projected")
	}
	overlay := filepath.Join(root, "overlays", "production")
	overlayKustomization, err := os.ReadFile(filepath.Join(overlay, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overlayKustomization), "external-secret.yaml") {
		t.Fatalf("overlay kustomization does not reference the projection: %s", overlayKustomization)
	}

	kustomizer := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rendered, err := kustomizer.Run(filesys.MakeFsOnDisk(), overlay)
	if err != nil {
		t.Fatalf("build service overlay: %v", err)
	}
	encoded, err := rendered.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	manifests, _, err := decodeYAML("overlay.yaml", encoded)
	if err != nil {
		t.Fatal(err)
	}
	var external *manifest
	for index := range manifests {
		if manifests[index].kind == "ExternalSecret" {
			external = &manifests[index]
		}
	}
	if external == nil {
		t.Fatalf("service overlay rendered no ExternalSecret: %d manifests", len(manifests))
	}
	if err := validateManifest(*external, nil, true); err != nil {
		t.Fatalf("projected ExternalSecret is not promotable: %v", err)
	}
}

func TestProjectServiceSecretsNoOpWithoutDeclaration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeServiceTree(t, root, "production")
	projected, err := projectServiceSecrets(root, "accounts", "production", "payments", nil)
	if err != nil {
		t.Fatal(err)
	}
	if projected {
		t.Fatal("projection ran without an environment declaration")
	}
	if _, err := os.Stat(filepath.Join(root, "overlays", "production", "external-secret.yaml")); !os.IsNotExist(err) {
		t.Fatalf("external-secret.yaml was written without a declaration: %v", err)
	}
}

// The renderers must invoke Workspace.ValidateEnvironments before doing any work,
// so a service-secrets override naming an unknown service fails fast instead of
// silently projecting the default <service>/<key> paths for a typo.
func TestRenderServiceRejectsUnknownServiceSecretOverride(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: platform
layout: modules
modules:
  - name: web
environments:
  - name: prod
    namespace: platform
    service-secrets:
      secret-store:
        name: azure-keyvault-prod
        kind: ClusterSecretStore
      services:
        ghost:
          remote-keys:
            client-secret: ghost/client-secret
`,
		filepath.Join("modules", "web", resources.ModuleConfigurationName): `kind: module
name: web
services:
    - name: web
`,
		filepath.Join("modules", "web", "services", "web", resources.ServiceConfigurationName): `kind: service
name: web
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
`,
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	env := workspace.FindEnvironment("prod")
	if env == nil {
		t.Fatal("prod environment did not load")
	}
	_, err = RenderService(ctx, workspace, &resources.Module{Name: "web"}, &resources.Service{Name: "web"}, env, "", false, nil)
	if err == nil {
		t.Fatal("expected RenderService to reject the unknown service-secrets override")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error = %v, want it to name the unknown service", err)
	}
}

func TestProjectServiceSecretsFailsClearlyWithoutOverlay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	writeServiceTree(t, root, "production")
	if err := os.RemoveAll(filepath.Join(root, "overlays")); err != nil {
		t.Fatal(err)
	}
	_, err := projectServiceSecrets(root, "accounts", "production", "payments", azureServiceSecrets())
	if err == nil {
		t.Fatal("expected an error when the environment overlay is missing")
	}
	if !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("error = %v, want it to name the missing overlay", err)
	}
}

func TestProjectRenderedServiceSecretsCoversEveryServiceTree(t *testing.T) {
	stage := t.TempDir()
	accounts := filepath.Join(stage, "modules", "identity", "services", "accounts")
	writeServiceTree(t, accounts, "production")
	// A dependency that references no secret-<service> must not gain a projection.
	web := filepath.Join(stage, "modules", "identity", "services", "web")
	if err := os.MkdirAll(filepath.Join(web, "overlays", "production"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "overlays", "production", "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := &resources.Environment{Name: "production", Namespace: "payments", ServiceSecrets: azureServiceSecrets()}
	if err := projectRenderedServiceSecrets(stage, env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(accounts, "overlays", "production", "external-secret.yaml")); err != nil {
		t.Fatalf("accounts projection missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(web, "overlays", "production", "external-secret.yaml")); !os.IsNotExist(err) {
		t.Fatalf("web gained a projection despite referencing no secret: %v", err)
	}
}

func TestProjectRenderedServiceSecretsNoOpWithoutDeclaration(t *testing.T) {
	stage := t.TempDir()
	writeServiceTree(t, filepath.Join(stage, "modules", "identity", "services", "accounts"), "production")
	env := &resources.Environment{Name: "production", Namespace: "payments"}
	if err := projectRenderedServiceSecrets(stage, env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stage, "modules", "identity", "services", "accounts", "overlays", "production", "external-secret.yaml")); !os.IsNotExist(err) {
		t.Fatalf("projection ran without a declaration: %v", err)
	}
}

func promotableManagedOutput() *InventoryKubernetesOutput {
	return &InventoryKubernetesOutput{
		Kind:            "KUSTOMIZE",
		Profile:         "KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1",
		ContractVersion: "codefly.dev/kubernetes-manifest/v1",
		Validation: &InventoryKubernetesValidation{
			StaticValidation: "STATUS_PASSED", ServerSideValidation: "STATUS_PASSED",
			Promotable: true, Violations: []string{},
		},
	}
}

// A managed service that declares only secret references now flips to a bootstrap
// unit (Path + Output set), so the inventory contract must accept that shape —
// with the deployment evidence that renderModuleTree records from the flow — and
// still reject it when that evidence is missing.
func TestValidateInventoryUnitsGovernSecretsBearingManagedBootstrapUnit(t *testing.T) {
	unit := InventoryUnit{
		Kind: UnitKindService, Module: "payments", Name: "store",
		Managed: true, Bootstrap: true,
		Path:   filepath.ToSlash(filepath.Join("services", "store")),
		Output: promotableManagedOutput(),
	}
	inventory := &Inventory{
		SchemaVersion: SchemaVersion, Module: "payments",
		OwnedPath: "environments/deployments/modules/payments",
		Units:     []InventoryUnit{unit},
	}
	if err := validateInventoryUnits(inventory); err != nil {
		t.Fatalf("secrets-bearing managed bootstrap unit rejected: %v", err)
	}

	inventory.Units[0].Output = nil
	if err := validateInventoryUnits(inventory); err == nil {
		t.Fatal("managed bootstrap unit without deployment evidence was accepted")
	}
}
