package gitops

import (
	"github.com/codefly-dev/core/solutionhost"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
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
	return module
}

func identityEnvironment() *environments.Environment {
	return &environments.Environment{
		Name:      "prod",
		Namespace: "obin",
		Host:      testHost(),
		ServiceIdentity: &environments.EnvironmentServiceIdentity{
			Default: &environments.EnvironmentWorkloadIdentity{Principal: "crm@obin.iam.example"},
		},
	}
}

func TestSolutionInstanceComesFromTheModuleTheRenderResolved(t *testing.T) {
	module := solutionModule(t, "crm", "api")
	services := []*resources.Service{{
		Name:      "api",
		Endpoints: []*resources.Endpoint{{Name: "grpc", API: "grpc", Visibility: "internal"}},
	}}
	options := &RenderOptions{
		Module: "crm", Environment: "prod", Workspace: "obin",
		Package: &InventoryPackage{ID: "obin/crm", Version: "1.4.0"},
		Units:   promotableServiceGraph("crm", []string{"api"}),
	}
	instance, undeclared, err := presenceInstanceOf(module, services, identityEnvironment(), options)
	if err != nil {
		t.Fatal(err)
	}
	if instance == nil {
		t.Fatalf("a module shipping a solution manifest is not a solution instance: %s", undeclared)
	}
	if instance.Kind != solutionhost.KindSolution || instance.Name != "crm" || instance.Alias != "crm" {
		t.Fatalf("instance identity %+v", instance)
	}
	if !strings.HasPrefix(string(instance.ReleaseDigest), "sha256:") {
		t.Fatalf("release digest %q", instance.ReleaseDigest)
	}
	if instance.Package != "obin/crm" || instance.Version != "1.4.0" {
		t.Fatalf("instance release %+v", instance)
	}
	if len(instance.Units) != 1 || instance.Units[0].Path != "services/api" || instance.Units[0].Subject != "crm@obin.iam.example" {
		t.Fatalf("instance units %+v", instance.Units)
	}
	if len(instance.Endpoints) != 1 || instance.Endpoints[0].Module != "crm" || instance.Endpoints[0].Service != "api" {
		t.Fatalf("instance endpoints %+v", instance.Endpoints)
	}
	if len(instance.Modules) != 1 || instance.Modules[0].Package != "obin/crm" {
		t.Fatalf("instance module pins %+v", instance.Modules)
	}
}

// TestAModuleShippingNoSolutionManifestIsAModuleInstance pins presence for
// modules: an ordinary module declares its presence with kind module and no
// route, and a module with no package manifest names no release and declares
// nothing, with the reason reported.
func TestAModuleShippingNoSolutionManifestIsAModuleInstance(t *testing.T) {
	module := &resources.Module{Name: "payments"}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "module.codefly.yaml"), []byte("kind: module\nname: payments\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	module.WithDir(dir)
	instance, undeclared, err := presenceInstanceOf(module, nil, identityEnvironment(), &RenderOptions{Module: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if instance != nil || !strings.Contains(undeclared, "no package manifest") {
		t.Fatalf("a module without a package was declared (%+v) or the reason was not reported (%q)", instance, undeclared)
	}
	instance, undeclared, err = presenceInstanceOf(module, nil, identityEnvironment(), &RenderOptions{
		Module: "payments", Package: &InventoryPackage{ID: "example/payments", Version: "2.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance == nil || undeclared != "" {
		t.Fatalf("a packaged module declares its presence: %+v %q", instance, undeclared)
	}
	if instance.Kind != solutionhost.KindModule || instance.Alias != "" || instance.Name != "payments" {
		t.Fatalf("module instance %+v", instance)
	}
}

func TestSolutionInstanceRefusesAnEndpointWithNoDeclaredAPI(t *testing.T) {
	module := solutionModule(t, "crm", "api")
	services := []*resources.Service{{
		Name:      "api",
		Endpoints: []*resources.Endpoint{{Name: "mystery"}},
	}}
	_, _, err := presenceInstanceOf(module, services, identityEnvironment(), &RenderOptions{Module: "crm", Package: &InventoryPackage{ID: "obin/crm", Version: "1.4.0"}})
	if err == nil || !strings.Contains(err.Error(), "declares no api") {
		t.Fatalf("an endpoint with no protocol was declared to the host: %v", err)
	}
}

// TestTwoInstancesOfOneSolutionAreTwoBindings is the distinctness requirement
// read off the composition rather than off bindingID: two composed modules
// shipping the same solution manifest are two instances.
func TestTwoInstancesOfOneSolutionAreTwoBindings(t *testing.T) {
	env := identityEnvironment()
	options := &RenderOptions{Environment: "prod", Workspace: "obin", Package: &InventoryPackage{ID: "obin/crm", Version: "1.4.0"}}
	first, _, err := presenceInstanceOf(solutionModule(t, "crm", "api"), nil, env, options)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := presenceInstanceOf(solutionModule(t, "crm-eu", "api"), nil, env, options)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := bindingID(options.Workspace, options.Environment, first.Name)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := bindingID(options.Workspace, options.Environment, second.Name)
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatalf("two instances of one solution share binding ID %q", firstID)
	}
	if first.Alias == second.Alias {
		t.Fatalf("two instances of one solution claim the same route alias %q", first.Alias)
	}
}
