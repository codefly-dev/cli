package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/solutionhost"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	solutionmanifest "github.com/codefly-dev/core/solution/manifest"
)

const crmSolutionManifest = `
schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  kind: codefly:solution
  publisher: obin
  name: crm
  version: 1.4.0
api:
  exposes:
    - id: gateway
      protocol: http
lifecycle:
  create: true
`

// solutionModule is a composed solution as a module render loads it: a module
// whose directory ships a solution manifest.
func solutionModule(t *testing.T, name, entry string) *resources.Module {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, solutionmanifest.FileName), []byte(crmSolutionManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	module := &resources.Module{Name: name, ServiceEntry: entry}
	module.WithDir(dir)
	// The frontend the solution declares beside its entry service, which its
	// manifest carries.
	entryDir := filepath.Join(dir, "services", entry)
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entryDir, solutionrun.FrontendFileName), []byte("title: CRM\norder: 1\nexposed_module: ./Page\nhost_contract: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return module
}

func identityEnvironment() *environments.Environment {
	return &environments.Environment{Name: "prod", Namespace: "obin", Host: testHost()}
}

// instanceOf is moduleInstanceOf over a composition that supplies no
// configuration, for a module whose request needs none.
func instanceOf(t *testing.T, module *resources.Module, services []*resources.Service, pkg *InventoryPackage, carriers map[string]string) (*ModuleInstance, string, error) {
	t.Helper()
	return moduleInstanceOf(context.Background(), &resources.Workspace{Name: "example"}, module, services, identityEnvironment(), pkg, nil, carriers)
}

func TestSolutionInstanceComesFromTheModuleTheRenderResolved(t *testing.T) {
	module := solutionModule(t, "crm", "api")
	services := []*resources.Service{{
		Name: "api", ModuleIdentity: true,
		Endpoints: []*resources.Endpoint{{Name: "grpc", API: "grpc", Visibility: "internal"}},
	}}
	carriers := map[string]string{
		resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: "crm", Service: "api", Name: "grpc", API: "grpc"}): "api.crm.svc.cluster.local:9090",
	}
	instance, undeclared, err := instanceOf(t, module, services, &InventoryPackage{ID: "obin/crm", Version: "1.4.0"}, carriers)
	if err != nil {
		t.Fatal(err)
	}
	if instance == nil {
		t.Fatalf("a module shipping a solution manifest is not a solution instance: %s", undeclared)
	}
	if instance.Kind != solutionhost.KindSolution || instance.Name != "crm" || instance.Package != "obin/crm" || instance.Version != "1.4.0" {
		t.Fatalf("instance identity %+v", instance)
	}
	if len(instance.Identities) != 1 || instance.Identities[0] != (IdentityWorkload{Service: "api"}) {
		t.Fatalf("instance identities %+v", instance.Identities)
	}
	if len(instance.Endpoints) != 1 || instance.Endpoints[0] != (solutionhost.Endpoint{Name: "grpc", Service: "api", API: "grpc", Address: "api.crm.svc.cluster.local:9090"}) {
		t.Fatalf("instance endpoints %+v", instance.Endpoints)
	}
	if instance.Frontend == nil || instance.Authority == nil || instance.Authority.Principal != "crm" {
		t.Fatalf("instance frontend %+v authority %+v", instance.Frontend, instance.Authority)
	}
}

// TestAModuleShippingNoSolutionManifestIsAModuleInstance: an ordinary module
// declares kind module, its name its alias and principal; a module with no
// package manifest names no release, and one with no identity service runs as
// nothing — each declares no manifest, with the reason reported.
func TestAModuleShippingNoSolutionManifestIsAModuleInstance(t *testing.T) {
	module := &resources.Module{Name: "payments"}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "module.codefly.yaml"), []byte("kind: module\nname: payments\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	module.WithDir(dir)
	identity := []*resources.Service{{Name: "api", ModuleIdentity: true}}
	instance, undeclared, err := instanceOf(t, module, identity, nil, nil)
	if err != nil || instance != nil || !strings.Contains(undeclared, "no package manifest") {
		t.Fatalf("a module without a package was declared (%+v, %v) or the reason was not reported (%q)", instance, err, undeclared)
	}
	pkg := &InventoryPackage{ID: "example/payments", Version: "2.0.0"}
	instance, undeclared, err = instanceOf(t, module, []*resources.Service{{Name: "api"}}, pkg, nil)
	if err != nil || instance != nil || !strings.Contains(undeclared, "declares no module-identity service") {
		t.Fatalf("a module nothing runs as was declared (%+v, %v) or the reason was not reported (%q)", instance, err, undeclared)
	}
	instance, undeclared, err = instanceOf(t, module, identity, pkg, nil)
	if err != nil || instance == nil || undeclared != "" {
		t.Fatalf("a packaged module declares its instance: %+v %q %v", instance, undeclared, err)
	}
	if instance.Kind != solutionhost.KindModule || instance.Name != "payments" || instance.Authority.Principal != "payments" {
		t.Fatalf("module instance %+v", instance)
	}
}

// TestTwoInstancesOfOneSolutionAreTwoBindings is the distinctness requirement
// read off the composition rather than off bindingID: two composed modules
// shipping the same solution manifest are two instances.
func TestTwoInstancesOfOneSolutionAreTwoBindings(t *testing.T) {
	pkg := &InventoryPackage{ID: "obin/crm", Version: "1.4.0"}
	identity := []*resources.Service{{Name: "api", ModuleIdentity: true}}
	first, _, err := instanceOf(t, solutionModule(t, "crm", "api"), identity, pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := instanceOf(t, solutionModule(t, "crm-eu", "api"), identity, pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := bindingID("obin", "prod", first.Name)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := bindingID("obin", "prod", second.Name)
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID || first.Name == second.Name {
		t.Fatalf("two instances of one solution share binding %q or alias %q", firstID, first.Name)
	}
}
