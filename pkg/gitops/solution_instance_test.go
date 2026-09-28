package gitops

import (
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
	instance, err := solutionInstanceOf(module, services, identityEnvironment(), options)
	if err != nil {
		t.Fatal(err)
	}
	if instance == nil {
		t.Fatal("a module shipping a solution manifest is not a solution instance")
	}
	if instance.Name != "crm" || instance.Alias != "crm" {
		t.Fatalf("instance identity %+v", instance)
	}
	if instance.Package != "obin/crm" || instance.Version != "1.4.0" {
		t.Fatalf("instance release %+v", instance)
	}
	if instance.Subject != "crm@obin.iam.example" {
		t.Fatalf("instance subject %q", instance.Subject)
	}
	if len(instance.Units) != 1 || instance.Units[0].Path != "services/api" {
		t.Fatalf("instance units %+v", instance.Units)
	}
	if len(instance.Endpoints) != 1 || instance.Endpoints[0].Module != "crm" || instance.Endpoints[0].Service != "api" {
		t.Fatalf("instance endpoints %+v", instance.Endpoints)
	}
	if len(instance.Modules) != 1 || instance.Modules[0].Package != "obin/crm" {
		t.Fatalf("instance module pins %+v", instance.Modules)
	}
}

func TestAModuleShippingNoSolutionManifestIsNotAnInstance(t *testing.T) {
	module := &resources.Module{Name: "payments"}
	module.WithDir(t.TempDir())
	instance, err := solutionInstanceOf(module, nil, identityEnvironment(), &RenderOptions{Module: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if instance != nil {
		t.Fatalf("an ordinary module was declared a solution instance: %+v", instance)
	}
}

func TestSolutionInstanceRefusesAnEndpointWithNoDeclaredAPI(t *testing.T) {
	module := solutionModule(t, "crm", "api")
	services := []*resources.Service{{
		Name:      "api",
		Endpoints: []*resources.Endpoint{{Name: "mystery"}},
	}}
	_, err := solutionInstanceOf(module, services, identityEnvironment(), &RenderOptions{Module: "crm"})
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
	first, err := solutionInstanceOf(solutionModule(t, "crm", "api"), nil, env, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := solutionInstanceOf(solutionModule(t, "crm-eu", "api"), nil, env, options)
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
