package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateArgoBootstrapEmitsTenantApplicationSet(t *testing.T) {
	root := t.TempDir()
	revision := strings.Repeat("b", 40)
	inventory := &Inventory{
		SchemaVersion: SchemaVersion,
		Module:        "payments",
		Environment:   "production",
		Namespace:     "payments",
		AppProject:    "payments-production",
		ModulePath:    "module",
		Units: []InventoryUnit{
			{Kind: UnitKindService, Module: "payments", Name: "api", Path: "services/api"},
			{Kind: UnitKindService, Module: "payments", Name: "store", Path: "services/store"},
		},
	}
	for _, component := range []string{"module", "services/api", "services/store"} {
		writeOverlay(t, filepath.Join(root, component, "overlays", "production"))
	}
	config := &repositoryConfig{RepoURL: "https://github.com/codefly-dev/manifests.git"}
	targetPath := "environments/deployments/modules/payments"
	if err := generateArgoBootstrap(
		context.Background(), config, root, targetPath, inventory, "production", revision, "",
	); err != nil {
		t.Fatal(err)
	}

	bootstrap := filepath.Join(root, "bootstrap")
	data, err := os.ReadFile(filepath.Join(bootstrap, "applicationset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set := string(data)
	for _, want := range []string{
		"kind: ApplicationSet",
		"targetRevision: " + revision,
		"revision: HEAD",
		"{{ .server }}",
		"{{ .component }}",
		"{{ .wave }}",
		"{{ .overlay }}",
		"overlay: " + targetPath + "/module/overlays/production",
		"overlay: " + targetPath + "/services/api/overlays/production",
		"overlay: " + targetPath + "/services/store/overlays/production",
	} {
		if !strings.Contains(set, want) {
			t.Fatalf("ApplicationSet missing %q:\n%s", want, set)
		}
	}

	// The in-cluster tenant is built into the ApplicationSet as a list generator,
	// so today's single-cluster fan-out never depends on repository state.
	if !strings.Contains(set, "tenant: "+defaultTenant) ||
		!strings.Contains(set, "server: "+inClusterServer) {
		t.Fatalf("in-cluster tenant not built in:\n%s", set)
	}

	// The tenant registry the generator discovers must live OUTSIDE the module's
	// publication path, so re-publishing the module never wipes an operator's
	// tenant folder nor stages its deletion (which Argo would prune). The driver
	// also writes no registry file of its own.
	registryGlob := "tenants/production/*/cluster.json"
	if !strings.Contains(set, registryGlob) {
		t.Fatalf("registry generator glob %q missing:\n%s", registryGlob, set)
	}
	if strings.Contains(set, targetPath+"/tenants") || strings.Contains(set, targetPath+"/bootstrap/tenants") {
		t.Fatalf("tenant registry must not live under the module path:\n%s", set)
	}
	if _, err := os.Stat(filepath.Join(bootstrap, "tenants")); !os.IsNotExist(err) {
		t.Fatalf("driver must not write a tenant registry into the wiped bootstrap tree: %v", err)
	}

	// The stamped Application name reserves a fixed tenant budget so it can never
	// exceed the 63-char Kubernetes limit for any runtime tenant folder name.
	if !strings.Contains(set, "trunc 20") {
		t.Fatalf("Application name does not bound the tenant budget:\n%s", set)
	}

	// The bootstrap revision and service-graph gates read the ApplicationSet the
	// same way they read the per-service Applications it replaced.
	got, err := bootstrapRevision(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if got != revision {
		t.Fatalf("bootstrap revision = %q, want %q", got, revision)
	}
	if err := validateBootstrapRevision(bootstrap, revision); err != nil {
		t.Fatal(err)
	}
	if err := validateBootstrapUnits(bootstrap, targetPath, inventory, "production"); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateArgoBootstrapStampsSolutionUnit(t *testing.T) {
	root := t.TempDir()
	revision := strings.Repeat("c", 40)
	targetPath := "environments/deployments/modules/hello"
	inventory := &Inventory{
		SchemaVersion: SchemaVersion,
		Module:        "hello",
		Environment:   "local",
		Namespace:     "hello",
		AppProject:    "hello",
		Units: []InventoryUnit{
			{Kind: UnitKindSolution, Module: "hello", Name: "hello", Path: "solutions/hello"},
		},
	}
	writeOverlay(t, filepath.Join(root, "solutions", "hello", "overlays", "local"))
	config := &repositoryConfig{RepoURL: "https://github.com/codefly-dev/manifests.git"}
	if err := generateArgoBootstrap(
		context.Background(), config, root, targetPath, inventory, "local", revision, "",
	); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(root, "bootstrap", "applicationset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set := string(data)
	if !strings.Contains(set, "overlay: "+targetPath+"/solutions/hello/overlays/local") {
		t.Fatalf("ApplicationSet does not stamp the solution overlay:\n%s", set)
	}
	// A solution reaches ArgoCD through the same bootstrap contract as a service:
	// its Application is discovered by the graph gates that back publish/observe.
	if err := validateBootstrapUnits(filepath.Join(root, "bootstrap"), targetPath, inventory, "local"); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateArgoBootstrapBoundsApplicationName(t *testing.T) {
	root := t.TempDir()
	revision := strings.Repeat("c", 40)
	longModule := "platform-observability-and-telemetry"
	longService := "metrics-aggregation-and-rollup-pipeline"
	inventory := &Inventory{
		SchemaVersion: SchemaVersion,
		Module:        longModule,
		Environment:   "production",
		Namespace:     "obs",
		AppProject:    "obs-production",
		Units: []InventoryUnit{
			{Kind: UnitKindService, Module: longModule, Name: longService, Path: "services/" + longService},
		},
	}
	writeOverlay(t, filepath.Join(root, "services", longService, "overlays", "production"))
	config := &repositoryConfig{RepoURL: "https://github.com/codefly-dev/manifests.git"}
	if err := generateArgoBootstrap(
		context.Background(), config, root, "environments/deployments/modules/"+longModule, inventory, "production", revision, "",
	); err != nil {
		t.Fatal(err)
	}
	// The longest realistic tenant folder name plus the bounded component must stay
	// within the 63-char limit; the old "<tenant>-<component>" concatenation of two
	// full-length strings would blow past it.
	component := argoBoundedName(componentNameBudget, longModule, longService)
	if len(component) > componentNameBudget {
		t.Fatalf("component %q (%d) exceeds budget %d", component, len(component), componentNameBudget)
	}
	worstTenant := strings.Repeat("t", tenantNameBudget)
	if got := len(worstTenant) + 1 + len(component); got > 63 {
		t.Fatalf("worst-case Application name length %d exceeds 63", got)
	}
}

func writeOverlay(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: overlay\n  namespace: payments\n"
	if err := os.WriteFile(filepath.Join(dir, "configmap.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	kustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - configmap.yaml\n"
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomization), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateApplicationSetRejectsForeignProject(t *testing.T) {
	item := manifest{
		group: argoAPIGroup,
		kind:  "ApplicationSet",
		path:  "bootstrap/applicationset.yaml",
		value: map[string]any{
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{"project": "someone-else"},
				},
			},
		},
	}
	if err := validateApplicationSet(item, &projectContract{name: "payments-production"}); err == nil {
		t.Fatal("expected ApplicationSet template project mismatch to be rejected")
	}
}

func TestBootstrapApplicationSetExemptionIsNarrow(t *testing.T) {
	appSet := []manifest{{group: argoAPIGroup, kind: "ApplicationSet"}}
	if !isBootstrapApplicationSet(bootstrapApplicationSetPath, appSet) {
		t.Fatal("the CLI-owned bootstrap ApplicationSet must be exempt")
	}
	// An ApplicationSet anywhere other than the bootstrap path must NOT be exempt,
	// so a service overlay cannot smuggle Go-template placeholders past validation.
	if isBootstrapApplicationSet("services/api/overlays/production/rogue.yaml", appSet) {
		t.Fatal("an ApplicationSet outside the bootstrap path must not be exempt")
	}
	if isBootstrapApplicationSet(bootstrapApplicationSetPath, nil) {
		t.Fatal("empty decode must not be exempt from the placeholder rule")
	}
	if isBootstrapApplicationSet(bootstrapApplicationSetPath, []manifest{{group: argoAPIGroup, kind: "Application"}}) {
		t.Fatal("an Application must not be exempt")
	}
	mixed := []manifest{
		{group: argoAPIGroup, kind: "ApplicationSet"},
		{group: "", kind: "ConfigMap"},
	}
	if isBootstrapApplicationSet(bootstrapApplicationSetPath, mixed) {
		t.Fatal("a file mixing an ApplicationSet with other manifests must not be exempt")
	}
	// The manifest-path gate that routes validateManifest recognizes both the raw
	// bootstrap file and the Kustomize-rendered bootstrap output, tolerates the
	// #document suffix, and rejects anything rendered from another directory.
	if !isBootstrapApplicationSetPath(bootstrapApplicationSetPath + "#1") {
		t.Fatal("bootstrap ApplicationSet manifest path must be recognized")
	}
	if !isBootstrapApplicationSetPath("kustomize:bootstrap/rendered.yaml#2") {
		t.Fatal("Kustomize-rendered bootstrap output must be recognized")
	}
	if isBootstrapApplicationSetPath("kustomize:services/api/overlays/production/rendered.yaml#2") {
		t.Fatal("a service overlay ApplicationSet must not be recognized as CLI-owned")
	}
	if isBootstrapApplicationSetPath("services/api/overlays/production/rogue.yaml#1") {
		t.Fatal("non-bootstrap ApplicationSet manifest path must not be recognized")
	}
}
