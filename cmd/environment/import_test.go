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

const fixtureContract = `{
  "schema": "codefly/cell/v2",
  "cell": "hosted-eastus2",
  "coordinate": "hosted-eastus2",
  "environment": {
    "name": "azure",
    "namespace": "acme",
    "cluster": {"kind": "aks", "context": "hosted-eastus2"},
    "dns": {"app-host-suffix": "staging.example"},
    "registry": {"url": "x.azurecr.io"},
    "managed-services": {"store": {
      "kind": "azure-postgres-flexible",
      "external-name": "p.postgres.database.azure.com",
      "port": 5432,
      "egress-cidrs": ["10.20.11.0/28"]
    }},
    "service-secrets": {"secret-store": {"name": "cell-secrets", "kind": "ClusterSecretStore"}},
    "gitops": {"repo-url": "https://github.com/x/fleet.git", "branch": "main", "path": "workloads/hosted/staging/acme"}
  }
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
	if opts.contractData == nil {
		opts.contractData = []byte(fixtureContract)
	}
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
	if !ok || mapping.RemoteKeys["db-password"].Key != "accounts/db-password" {
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

func TestImportRejectsRetargetingWithoutWriting(t *testing.T) {
	dir := writeWorkspace(t, existingAzureWorkspace)
	path := filepath.Join(dir, resources.WorkspaceConfigurationName)
	before := readFile(t, path)
	err := runImport(t.Context(), &importOptions{
		dir: dir, envName: "azure", namespace: "another", namespaceSet: true,
		contractData: []byte(fixtureContract), stdout: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match declared target") {
		t.Fatalf("expected target rejection, got %v", err)
	}
	if got := readFile(t, path); got != before {
		t.Fatal("target rejection modified workspace")
	}
}

func TestImportCarriesGenericFieldsIntoExistingEnvironment(t *testing.T) {
	dir := writeWorkspace(t, existingAzureWorkspace)
	contract := `{"schema":"codefly/cell/v2","environment":{
	  "name":"azure","namespace":"acme","configuration-profile":"staging",
	  "managed-services":{"store":{
	    "kind":"external","external-name":"endpoint.example","port":8443,
	    "identity":{"principal":"runtime","annotations":{"identity":"runtime"},"labels":{"enabled":"true"}},
	    "secret-references":[]
	  }},
	  "gitops":{"repo-url":"https://example.com/owner/delivery","path":"reviewed/app","branch":"release"}
	}}`
	doImport(t, dir, importOptions{contractData: []byte(contract)})
	env := loadWorkspace(t, dir).FindEnvironment("azure")
	store := env.ManagedServices["store"]
	if store.Port != 8443 || store.Identity == nil || store.Identity.Principal != "runtime" || store.Identity.Annotations["identity"] != "runtime" || store.Identity.Labels["enabled"] != "true" {
		t.Fatalf("generic endpoint declaration lost fields: %+v", store)
	}
	if len(store.SecretReferences) != 0 || env.Gitops.Branch != "release" {
		t.Fatalf("explicit declarations did not replace old values: %+v", env)
	}
	contract = strings.Replace(contract, `"annotations":{"identity":"runtime"}`, `"annotations":{}`, 1)
	doImport(t, dir, importOptions{contractData: []byte(contract)})
	if got := loadWorkspace(t, dir).FindEnvironment("azure").ManagedServices["store"].Identity.Annotations; len(got) != 0 {
		t.Fatalf("explicit empty map retained old annotations: %v", got)
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
	doImport(t, dir, importOptions{contractData: []byte(strings.Replace(fixtureContract, `"store":`, `"platform":`, 1))})
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

func TestImportPreservesOtherExplicitManagedServices(t *testing.T) {
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
	doImport(t, dir, importOptions{})
	services := loadWorkspace(t, dir).FindEnvironment("azure").ManagedServices
	if len(services) != 3 || services["db-a"].ExternalName != "a.example" || services["db-b"].ExternalName != "b.example" || services["store"].ExternalName != "p.postgres.database.azure.com" {
		t.Fatalf("explicit service names were not preserved: %+v", services)
	}
}

func TestImportUsesDeclaredNamespaceAndDeliveryPath(t *testing.T) {
	dir := writeWorkspace(t, "name: acme\nlayout: modules\n")
	contract := strings.Replace(fixtureContract, `"namespace": "acme"`, `"namespace": "prod-azure"`, 1)
	contract = strings.Replace(contract, "workloads/hosted/staging/acme", "reviewed/product", 1)
	doImport(t, dir, importOptions{namespace: "prod-azure", namespaceSet: true, contractData: []byte(contract)})
	ws := loadWorkspace(t, dir)
	env := ws.FindEnvironment("azure")
	if env.Gitops.Path != "reviewed/product" {
		t.Errorf("gitops path = %q, want declared reviewed/product", env.Gitops.Path)
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

// TestImportPreservesClusterKubeconfig is the regression test for the finding
// that importing replaced the cluster node wholesale, dropping the operator's
// kubeconfig path — a local fact the cell contract never carries. kind/context
// must update; kubeconfig must survive.
func TestImportPreservesClusterKubeconfig(t *testing.T) {
	src := `name: acme
layout: modules
environments:
    - name: azure
      cluster:
          kind: aks
          context: old-context
          kubeconfig: /home/op/.kube/prod-config
`
	dir := writeWorkspace(t, src)
	doImport(t, dir, importOptions{})
	ws := loadWorkspace(t, dir)
	env := ws.FindEnvironment("azure")
	if env.Cluster == nil || env.Cluster.Kubeconfig != "/home/op/.kube/prod-config" {
		t.Fatalf("cluster.kubeconfig dropped: %+v", env.Cluster)
	}
	if env.Cluster.Context != "hosted-eastus2" {
		t.Errorf("cluster.context = %q, want updated hosted-eastus2", env.Cluster.Context)
	}
}

// TestImportLeavesUnrelatedSoleManagedService is the regression test for the
// finding that a single managed service of a different kind (a cache) was
// silently rewritten as the contract's postgres database. The cache must be
// left intact and the database inserted beside it.
func TestImportLeavesUnrelatedSoleManagedService(t *testing.T) {
	src := `name: acme
layout: modules
environments:
    - name: azure
      managed-services:
          cache:
              kind: azure-redis
              external-name: cache.redis.example
              egress-cidrs:
                  - 10.99.0.0/28
`
	dir := writeWorkspace(t, src)
	doImport(t, dir, importOptions{})
	ws := loadWorkspace(t, dir)
	env := ws.FindEnvironment("azure")
	cache, ok := env.ManagedServices["cache"]
	if !ok {
		t.Fatalf("cache entry vanished: %v", env.ManagedServices)
	}
	if cache.Kind != "azure-redis" || cache.ExternalName != "cache.redis.example" {
		t.Errorf("unrelated cache was rewritten: %+v", cache)
	}
	store, ok := env.ManagedServices["store"]
	if !ok {
		t.Fatalf("database not inserted beside the cache; got %v", env.ManagedServices)
	}
	if store.Kind != "azure-postgres-flexible" {
		t.Errorf("inserted store has wrong kind: %+v", store)
	}
}

// TestImportIntoEmptyFlowSequence is the regression test for the finding that
// importing into `environments: []` appended a block sequence under the inline
// value, producing YAML that no longer loaded.
func TestImportIntoEmptyFlowSequence(t *testing.T) {
	for _, empty := range []string{"environments: []", "environments:"} {
		src := "name: acme\nlayout: modules\n" + empty + "\n"
		dir := writeWorkspace(t, src)
		doImport(t, dir, importOptions{})
		ws := loadWorkspace(t, dir) // fails the test if the written file is unparseable
		if ws.FindEnvironment("azure") == nil {
			t.Errorf("azure env not found after import into %q:\n%s", empty,
				readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName)))
		}
	}
}

// TestImportPreservesBlockScalarLastField is the regression test for EndLine
// under-counting a multi-line literal block scalar: when the imported item's
// last field is a `|` block, the splice must not strand or duplicate its lines
// and must leave the following environment intact.
func TestImportPreservesBlockScalarLastField(t *testing.T) {
	src := `name: acme
layout: modules
environments:
    - name: azure
      description: |
          first line
          second line
    - name: prod
      description: production
`
	dir := writeWorkspace(t, src)
	doImport(t, dir, importOptions{})
	got := readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName))
	if n := strings.Count(got, "second line"); n != 1 {
		t.Errorf("block scalar line count = %d, want 1 (stranded/duplicated splice):\n%s", n, got)
	}
	ws := loadWorkspace(t, dir)
	if p := ws.FindEnvironment("prod"); p == nil || p.Description != "production" {
		t.Errorf("following prod environment corrupted: %+v", p)
	}
	if a := ws.FindEnvironment("azure"); a == nil || a.Cluster == nil || a.Cluster.Kind != "aks" {
		t.Errorf("azure not imported over the block scalar: %+v", a)
	}
}

// TestImportWritesResolvedNamespace is the regression test for the finding that
// gitops.path baked in a namespace the file never declared: the resolved
// namespace must be persisted so path and namespace can never disagree.
func TestImportWritesResolvedNamespace(t *testing.T) {
	src := `name: acme
layout: modules
environments:
    - name: azure
      description: staging
`
	dir := writeWorkspace(t, src)
	doImport(t, dir, importOptions{}) // no --namespace: defaults to workspace name "acme"
	ws := loadWorkspace(t, dir)
	env := ws.FindEnvironment("azure")
	if env.Namespace != "acme" {
		t.Errorf("namespace = %q, want the resolved workspace name acme", env.Namespace)
	}
	if !strings.HasSuffix(env.Gitops.Path, "/"+env.Namespace) {
		t.Errorf("gitops.path %q does not agree with namespace %q", env.Gitops.Path, env.Namespace)
	}
}
