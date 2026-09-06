package environment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
)

// fixtureContract is a valid codefly/cell/v1 descriptor: one acr registry, one
// azure-postgres-flexible database, one ClusterSecretStore, a DNS suffix and a
// gitops prefix. It mirrors the shape infra-base's `obinctl cell-contract`
// emits.
const fixtureContract = `{
  "schema": "codefly/cell/v1",
  "cell": "hosted-eastus2",
  "coordinate": "hosted-eastus2",
  "cluster": {"kind": "aks", "name": "hosted-eastus2", "context": "hosted-eastus2"},
  "dns": {"app_host_suffix": "staging.example"},
  "registries": [{"kind": "acr", "url": "x.azurecr.io"}],
  "databases": [{
    "kind": "azure-postgres-flexible",
    "name": "platform",
    "fqdn": "p.postgres.database.azure.com",
    "egress_cidrs": ["10.20.11.0/28"],
    "database_names": ["users"]
  }],
  "secret_stores": [{"name": "cell-secrets", "kind": "ClusterSecretStore"}],
  "gitops": {"repo": "https://github.com/x/fleet.git", "workloads_path_prefix": "workloads/hosted/staging"}
}`

func writeWorkspace(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func doImport(t *testing.T, dir string, opts importOptions) string {
	t.Helper()
	opts.dir = dir
	opts.contractData = []byte(fixtureContract)
	if opts.envName == "" {
		opts.envName = "azure"
	}
	if opts.now.IsZero() {
		opts.now = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	}
	var out bytes.Buffer
	opts.stdout = &out
	if err := runImport(context.Background(), &opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	return out.String()
}

func loadWorkspace(t *testing.T, dir string) *resources.Workspace {
	t.Helper()
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	return ws
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestImportCreatesEnvironment(t *testing.T) {
	dir := writeWorkspace(t, "name: acme\nlayout: modules\n")

	doImport(t, dir, importOptions{})

	ws := loadWorkspace(t, dir)
	env := ws.FindEnvironment("azure")
	if env == nil {
		t.Fatal("environment azure not found after import")
	}
	if env.Cluster == nil || env.Cluster.Kind != "aks" {
		t.Fatalf("cluster kind = %+v, want aks", env.Cluster)
	}
	store, ok := env.ManagedServices["store"]
	if !ok {
		t.Fatalf("managed service store not found; got %v", env.ManagedServices)
	}
	if !reflect.DeepEqual(store.EgressCIDRs, []string{"10.20.11.0/28"}) {
		t.Fatalf("egress-cidrs = %v, want [10.20.11.0/28]", store.EgressCIDRs)
	}
	if env.ServiceSecrets == nil || env.ServiceSecrets.SecretStore.Name != "cell-secrets" {
		t.Fatalf("service-secrets secret-store = %+v, want cell-secrets", env.ServiceSecrets)
	}
	if env.Gitops == nil || env.Gitops.Path != "workloads/hosted/staging/acme" {
		t.Fatalf("gitops path = %+v, want workloads/hosted/staging/acme", env.Gitops)
	}
}

// existingAzureWorkspace declares an azure environment carrying operator-owned
// fields the import must preserve, plus a managed-services.store whose
// egress-cidrs the contract must update.
const existingAzureWorkspace = `name: acme
layout: modules
environments:
    - name: azure
      description: production azure # keep me
      cluster:
          kind: aks
          context: old-context
      ingress:
          - name: web
            service: frontend
            endpoint: http
            hosts:
                - app.example.com
          - name: api
            service: backend
            endpoint: grpc
            hosts:
                - api.example.com
      managed-services:
          store:
              kind: azure-postgres-flexible
              external-name: old.postgres.database.azure.com
              egress-cidrs:
                  - 10.0.0.0/28
              secret-references:
                  - name: secret-store
                    remote-key: acme/store
                    secret-store:
                        name: old-secrets
                        kind: ClusterSecretStore
      service-secrets:
          secret-store:
              name: old-secrets
              kind: ClusterSecretStore
          services:
              accounts:
                  remote-keys:
                      db-password: accounts/db-password
`

func TestImportPreservesOperatorFields(t *testing.T) {
	dir := writeWorkspace(t, existingAzureWorkspace)

	doImport(t, dir, importOptions{})

	content := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))
	if !strings.Contains(content, "# keep me") {
		t.Errorf("lost the # keep me comment:\n%s", content)
	}

	ws := loadWorkspace(t, dir)
	env := ws.FindEnvironment("azure")
	if env.Description != "production azure" {
		t.Errorf("description = %q, want %q", env.Description, "production azure")
	}
	if len(env.Ingress) != 2 {
		t.Errorf("ingress routes = %d, want 2", len(env.Ingress))
	}
	mapping, ok := env.ServiceSecrets.Services["accounts"]
	if !ok || mapping.RemoteKeys["db-password"] != "accounts/db-password" {
		t.Errorf("service-secrets.services.accounts.remote-keys not preserved: %+v", env.ServiceSecrets.Services)
	}
	store := env.ManagedServices["store"]
	if !reflect.DeepEqual(store.EgressCIDRs, []string{"10.20.11.0/28"}) {
		t.Errorf("egress-cidrs = %v, want updated [10.20.11.0/28]", store.EgressCIDRs)
	}
	if len(store.SecretReferences) != 1 || store.SecretReferences[0].RemoteKey != "acme/store" {
		t.Errorf("managed-service secret-references not preserved: %+v", store.SecretReferences)
	}
	// Contract-owned fields are updated.
	if store.ExternalName != "p.postgres.database.azure.com" {
		t.Errorf("external-name = %q, want updated", store.ExternalName)
	}
	if env.Cluster.Context != "hosted-eastus2" {
		t.Errorf("cluster context = %q, want updated", env.Cluster.Context)
	}
}

func TestImportRejectsWrongSchema(t *testing.T) {
	dir := writeWorkspace(t, "name: acme\nlayout: modules\n")
	opts := importOptions{
		dir:          dir,
		envName:      "azure",
		contractData: []byte(`{"schema": "obin-infra/cell-contract/v1", "cell": "x"}`),
		now:          time.Now(),
		stdout:       &bytes.Buffer{},
	}
	err := runImport(context.Background(), &opts)
	if err == nil || !strings.Contains(err.Error(), "unsupported cell-contract schema") {
		t.Fatalf("err = %v, want unsupported cell-contract schema", err)
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	dir := writeWorkspace(t, "name: acme\nlayout: modules\n")
	path := filepath.Join(dir, resources.WorkspaceConfigurationName)
	before := sha256.Sum256([]byte(readFile(t, path)))

	out := doImport(t, dir, importOptions{dryRun: true})

	after := sha256.Sum256([]byte(readFile(t, path)))
	if before != after {
		t.Error("dry-run modified workspace.codefly.yaml")
	}
	if !strings.Contains(out, "external-name:") || !strings.Contains(out, "+") {
		t.Errorf("dry-run diff does not show the added external-name field:\n%s", out)
	}
}

func TestImportRestampsProvenanceComment(t *testing.T) {
	dir := writeWorkspace(t, "name: acme\nlayout: modules\n")

	doImport(t, dir, importOptions{now: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)})
	doImport(t, dir, importOptions{now: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)})

	content := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))
	if n := strings.Count(content, provenanceMarker); n != 1 {
		t.Fatalf("provenance comment count = %d, want 1\n%s", n, content)
	}
}

// TestImportPreservesSurroundingBytes is the regression test for the whole-file
// reflow and for the splice-span ordering bug: importing one environment must
// leave every byte outside that environment's item untouched (other
// environments, top-level keys, blank lines, comments) and must not strand the
// original egress-cidrs lines it replaced.
func TestImportPreservesSurroundingBytes(t *testing.T) {
	src := `name: acme
layout: modules

modules:
    - name: backend

environments:
    - name: prod
      description: production
      cluster:
          kind: eks
          context: prod-ctx
    - name: azure
      description: staging
      managed-services:
          store:
              kind: azure-postgres-flexible
              external-name: old.example
              egress-cidrs:
                  - 10.0.0.0/28
`
	dir := writeWorkspace(t, src)
	doImport(t, dir, importOptions{})
	got := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))

	// Everything up to the azure item — the blank lines, modules, and the whole
	// prod environment — must be byte-identical.
	prefix := src[:strings.Index(src, "    - name: azure")]
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("surrounding bytes were reflowed.\nwant prefix:\n%q\n\ngot:\n%q", prefix, got)
	}
	if strings.Contains(got, "10.0.0.0/28") {
		t.Errorf("stale egress CIDR survived (stranded splice):\n%s", got)
	}
	if !strings.Contains(got, "10.20.11.0/28") {
		t.Errorf("egress CIDR not updated:\n%s", got)
	}
	ws := loadWorkspace(t, dir)
	if p := ws.FindEnvironment("prod"); p == nil || p.Cluster.Kind != "eks" {
		t.Errorf("prod environment altered: %+v", p)
	}
}

// TestImportUpdatesManagedServiceUnderOperatorKey covers the finding that
// ToEnvironment's fixed "store" key must not be forced onto a file whose
// operator declared the database under the service name it replaces — updating
// in place instead of adding a stale duplicate that keeps the wrong egress CIDR.
func TestImportUpdatesManagedServiceUnderOperatorKey(t *testing.T) {
	src := `name: acme
layout: modules
environments:
    - name: azure
      managed-services:
          platform:
              kind: azure-postgres-flexible
              external-name: old.example
              egress-cidrs:
                  - 10.0.0.0/28
`
	dir := writeWorkspace(t, src)
	doImport(t, dir, importOptions{})
	ws := loadWorkspace(t, dir)
	env := ws.FindEnvironment("azure")
	if _, dup := env.ManagedServices["store"]; dup {
		t.Errorf("added a duplicate 'store' entry instead of updating 'platform': %v", env.ManagedServices)
	}
	if len(env.ManagedServices) != 1 {
		t.Fatalf("expected exactly one managed service, got %d: %v", len(env.ManagedServices), env.ManagedServices)
	}
	p, ok := env.ManagedServices["platform"]
	if !ok || !reflect.DeepEqual(p.EgressCIDRs, []string{"10.20.11.0/28"}) {
		t.Errorf("platform entry not updated in place: %+v", env.ManagedServices)
	}
}

func TestImportRejectsAmbiguousManagedServices(t *testing.T) {
	src := `name: acme
layout: modules
environments:
    - name: azure
      managed-services:
          db-a:
              kind: azure-postgres-flexible
              external-name: a.example
              egress-cidrs:
                  - 10.0.0.0/28
          db-b:
              kind: azure-postgres-flexible
              external-name: b.example
              egress-cidrs:
                  - 10.0.1.0/28
`
	dir := writeWorkspace(t, src)
	path := filepath.Join(dir, resources.WorkspaceConfigurationName)
	before := readFile(t, path)
	err := runImport(context.Background(), &importOptions{
		dir: dir, envName: "azure", contractData: []byte(fixtureContract),
		now: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), stdout: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "managed services") {
		t.Fatalf("err = %v, want an ambiguity error mentioning managed services", err)
	}
	if readFile(t, path) != before {
		t.Error("file was modified despite the ambiguity error")
	}
}

// TestImportGitopsPathUsesNamespace pins the decision behind finding #4: the
// gitops path follows core's ToEnvironment, which derives it from the namespace
// (not the workspace name). They coincide by default; this asserts the
// namespace wins when --namespace makes them differ.
func TestImportGitopsPathUsesNamespace(t *testing.T) {
	dir := writeWorkspace(t, "name: acme\nlayout: modules\n")
	doImport(t, dir, importOptions{namespace: "prod-azure", namespaceSet: true})
	ws := loadWorkspace(t, dir)
	env := ws.FindEnvironment("azure")
	if env.Gitops.Path != "workloads/hosted/staging/prod-azure" {
		t.Errorf("gitops path = %q, want namespace-based workloads/hosted/staging/prod-azure", env.Gitops.Path)
	}
	if env.Namespace != "prod-azure" {
		t.Errorf("namespace = %q, want prod-azure", env.Namespace)
	}
}

func TestImportAppendsToExistingEnvironments(t *testing.T) {
	src := `name: acme
layout: modules
environments:
    - name: local
      description: dev
`
	dir := writeWorkspace(t, src)
	doImport(t, dir, importOptions{})
	got := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))
	if !strings.Contains(got, "    - name: local\n      description: dev\n") {
		t.Errorf("existing local env not preserved byte-for-byte:\n%s", got)
	}
	ws := loadWorkspace(t, dir)
	if ws.FindEnvironment("local") == nil || ws.FindEnvironment("azure") == nil {
		t.Errorf("expected both local and azure environments; got %d", len(ws.Environments))
	}
}
