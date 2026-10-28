package add

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

// withSolutionFlags sets the command's package-level flag state for one test
// and restores it, so tests do not leak into each other or into the binary's
// defaults.
func withSolutionFlags(t *testing.T, packageID string, consumes []string) {
	t.Helper()
	previous := struct {
		packageID, executor, title, minimum, authority string
		consumes                                       []string
		order                                          int64
	}{solutionPackageID, solutionExecutor, solutionTitle, solutionMinimumCodefly, solutionAuthorityIface, solutionConsumes, solutionOrder}
	t.Cleanup(func() {
		solutionPackageID, solutionExecutor, solutionTitle = previous.packageID, previous.executor, previous.title
		solutionMinimumCodefly, solutionAuthorityIface = previous.minimum, previous.authority
		solutionConsumes, solutionOrder = previous.consumes, previous.order
	})
	solutionPackageID, solutionConsumes = packageID, consumes
	solutionExecutor, solutionTitle = "", ""
	solutionOrder = 50
	solutionAuthorityIface = authorityInterfaceRange
	// A test binary reports no release version, which is exactly the case the
	// scaffold refuses to guess a floor for.
	solutionMinimumCodefly = ">=0.1.220 <1.0.0"
}

func documentsByName(t *testing.T, name string, consumes []string) map[string][]byte {
	t.Helper()
	withSolutionFlags(t, "obin/"+name, consumes)
	rendered, err := solutionDocuments(context.Background(), name)
	if err != nil {
		t.Fatalf("solutionDocuments: %v", err)
	}
	out := make(map[string][]byte, len(rendered))
	for _, document := range rendered {
		out[document.name] = document.content
	}
	return out
}

// The scaffold writes the four documents a solution is installed from, and the
// page declaration sits beside the ENTRY service — core reads it from there.
func TestSolutionScaffoldWritesTheDocumentsAHostInstallsFrom(t *testing.T) {
	documents := documentsByName(t, "wiki", nil)
	for _, want := range []string{
		manifest.FileName,
		modulecontract.FileName,
		composition.PackageManifestFileName,
		"services/" + solutionEntryService + "/" + solutionrun.FrontendFileName,
	} {
		if _, found := documents[want]; !found {
			t.Errorf("scaffold writes no %s", want)
		}
	}
}

// The package identity is required: a solution without one declares no
// presence, so the host never lists it and nobody can install it. Guessing an
// owner would commit a wrong identity, so the scaffold refuses instead.
func TestSolutionRefusesWithoutAPublishedIdentity(t *testing.T) {
	withSolutionFlags(t, "", nil)
	_, err := solutionDocuments(context.Background(), "wiki")
	if err == nil || !strings.Contains(err.Error(), "--package-id") {
		t.Fatalf("error = %v, want a refusal naming --package-id", err)
	}
}

// A build from source reports no release version. The package's Codefly floor
// cannot be derived from it, and a guess would claim compatibility with a
// release that may not exist, so it is refused by name.
func TestSolutionRefusesToGuessACodeflyFloorFromADevelopmentBuild(t *testing.T) {
	if _, err := codeflyFloor("development"); err == nil ||
		!strings.Contains(err.Error(), "--minimum-codefly-version") {
		t.Fatalf("error = %v, want a refusal naming --minimum-codefly-version", err)
	}
	floor, err := codeflyFloor("0.1.220")
	if err != nil {
		t.Fatal(err)
	}
	if floor != ">=0.1.220 <1.0.0" {
		t.Fatalf("floor = %q, want >=0.1.220 <1.0.0", floor)
	}
}

// The generated manifest names no executor — none is published — and is still
// read by the reader every render, sync and authority derivation uses. This is
// the whole claim behind shipping it without an agent.
func TestGeneratedSolutionManifestNamesNoExecutorAndStillLoads(t *testing.T) {
	documents := documentsByName(t, "wiki", []string{"connect:documents/documents/grpc"})
	content := documents[manifest.FileName]

	var decoded map[string]any
	if err := yaml.Unmarshal(content, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, named := decoded["agent"]; named {
		t.Fatalf("manifest names an executor; none is published:\n%s", content)
	}

	loaded, err := manifest.LoadConsumption(content)
	if err != nil {
		t.Fatalf("core's consumption reader refuses the generated manifest: %v\n%s", err, content)
	}
	if len(loaded.API.Consumes) != 1 || loaded.API.Consumes[0].Module != "documents" {
		t.Fatalf("consumption = %+v, want the one documents binding", loaded.API.Consumes)
	}
}

// Naming an executor makes the manifest pass core's STRICT loader outright,
// which is the state the file reaches the day an executor is published.
func TestGeneratedSolutionManifestWithAnExecutorPassesTheStrictLoader(t *testing.T) {
	withSolutionFlags(t, "obin/wiki", nil)
	executor := &resources.Agent{
		Kind: resources.SolutionAgent, Publisher: "codefly.dev", Name: "solution-generic", Version: "0.1.0",
	}
	content, err := marshalSolutionManifest(solutionManifest(nil, executor))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Load(content); err != nil {
		t.Fatalf("core's strict loader refuses a manifest naming an executor: %v\n%s", err, content)
	}
}

// Every rule of the schema but the executor's own is checked even when no
// executor is named — the stand-in exists so an unvalidated document is never
// what shipping without an agent costs.
func TestSolutionManifestRulesAreCheckedWithoutAnExecutor(t *testing.T) {
	built := solutionManifest([]manifest.APIDeclaration{
		{ID: "documents", Protocol: "connect", Module: "documents", Service: "documents"},
	}, nil)
	if _, err := marshalSolutionManifest(built); err == nil {
		t.Fatal("a half-bound consumed API was accepted; the stand-in is not validating the rest of the schema")
	}
}

// owner_aware is not a style choice: the authority issues a person a
// capability at a solution only when the solution declares owner-aware
// checking, and refuses it by name otherwise. A scaffold that omitted it would
// produce a solution that can act for nobody, silently.
func TestGeneratedContractDeclaresOwnerAwareChecking(t *testing.T) {
	documents := documentsByName(t, "wiki", nil)
	var contract modulecontract.Contract
	if err := yaml.Unmarshal(documents[modulecontract.FileName], &contract); err != nil {
		t.Fatal(err)
	}
	if !contract.OwnerAware {
		t.Fatal("the generated contract does not declare owner-aware checking, so the authority issues it no person capability")
	}
	if contract.Principal != "wiki" {
		t.Fatalf("principal = %q, want wiki", contract.Principal)
	}
	// Declared empty, never omitted: an absent list and "there are none" must
	// not look the same.
	for key, declared := range map[string]bool{
		"namespaces":     contract.Namespaces != nil,
		"queues":         contract.Queues != nil,
		"scope_ceilings": contract.ScopeCeilings != nil,
		"bindings":       contract.Bindings != nil,
	} {
		if !declared {
			t.Errorf("%s is omitted rather than declared empty", key)
		}
	}
}

// The scaffolded module and the published package disagree on purpose: the
// shell is scaffolded and composed, and is NOT published, because a host that
// federates the page's remote needs no shell of its own.
func TestThePackagePublishesTheBackendAndNotTheShell(t *testing.T) {
	documents := documentsByName(t, "wiki", nil)
	var packaged composition.PackageManifest
	if err := yaml.Unmarshal(documents[composition.PackageManifestFileName], &packaged); err != nil {
		t.Fatal(err)
	}
	if err := packaged.Validate(); err != nil {
		t.Fatalf("the generated package manifest is invalid: %v", err)
	}
	if len(packaged.Services) != 1 || packaged.Services[0].Name != solutionEntryService {
		t.Fatalf("published services = %+v, want only %s", packaged.Services, solutionEntryService)
	}
	// The contract ranges are derived from what core supports, never typed.
	if packaged.Contracts[composition.ContractComposition] != ">=2.0.0 <3.0.0" {
		t.Fatalf("composition contract = %q, want the range derived from core's supported majors",
			packaged.Contracts[composition.ContractComposition])
	}
}

// The page declaration is read back through core's own strict parser, which is
// the reader that will refuse it at delivery.
func TestGeneratedFrontendDeclarationParsesWithCore(t *testing.T) {
	documents := documentsByName(t, "last-login", nil)
	frontend, err := solutionhost.ParseFrontend(documents["services/"+solutionEntryService+"/"+solutionrun.FrontendFileName])
	if err != nil {
		t.Fatal(err)
	}
	if frontend.ExposedModule != "./Page" {
		t.Fatalf("exposed_module = %q, want ./Page", frontend.ExposedModule)
	}
	// A legible placeholder rather than the raw slug in the host's shell.
	if frontend.Title != "Last Login" {
		t.Fatalf("title = %q, want Last Login", frontend.Title)
	}
}

func TestParseConsumedAPI(t *testing.T) {
	declaration, err := parseConsumedAPI("connect:documents/documents/grpc")
	if err != nil {
		t.Fatal(err)
	}
	// The id and the route name default to the producing module's name, and
	// both are required by the consumer repository's own gate.
	if declaration.ID != "documents" || declaration.As != "documents" || declaration.Protocol != "connect" {
		t.Fatalf("declaration = %+v", declaration)
	}
	named, err := parseConsumedAPI("store=connect:documents/documents/grpc")
	if err != nil {
		t.Fatal(err)
	}
	if named.ID != "store" || named.As != "store" || named.Module != "documents" {
		t.Fatalf("declaration = %+v", named)
	}
	for _, malformed := range []string{"documents/documents/grpc", "connect:documents/documents", "connect:a/b/c/d"} {
		if _, err := parseConsumedAPI(malformed); err == nil {
			t.Errorf("parseConsumedAPI(%q) was accepted", malformed)
		}
	}
}

// The entry service declares the host's door and NOTHING else. A
// service-dependency on a consumed module renders into a mesh allow rule
// straight to that module's service, past the authority's check — which is the
// rule cmd/sync/solution_sdk.go already applies and the consumer repository's
// check-solution-mesh gate enforces.
func TestEntryServiceDeclaresTheAuthorityDoorAndNoModuleEdge(t *testing.T) {
	dependencies := solutionEntryDependencies(authorityInterfaceRange)
	if len(dependencies) != 2 {
		t.Fatalf("dependencies = %+v, want the interface and the hop", dependencies)
	}
	if dependencies[0].Interface != authorityInterfaceRange {
		t.Fatalf("first dependency = %+v, want the authority interface", dependencies[0])
	}
	hop := dependencies[1]
	if hop.Module != "authority" || hop.Name != "hop" || hop.Kind != resources.DependencyKindExternal {
		t.Fatalf("second dependency = %+v, want authority/hop as external", hop)
	}
	// What the scaffold writes must decode as core's own declaration.
	rendered, err := yaml.Marshal(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []resources.ServiceDependency
	if err := yaml.Unmarshal(rendered, &decoded); err != nil {
		t.Fatalf("the authored dependencies do not decode as core's: %v\n%s", err, rendered)
	}
	if len(decoded) != 2 || decoded[1].Endpoints[0].Name != "grpc" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// The entry service's manifest keeps whatever the agent wrote that the shape
// does not speak to, and gains exactly what the shape states.
func TestApplyEntryServiceShapeEditsInPlace(t *testing.T) {
	root := t.TempDir()
	module := &resources.Module{Name: "wiki"}
	module.WithDir(root)
	scaffolded := filepath.Join(root, "services", solutionEntryService, resources.ServiceConfigurationName)
	writeAddTestFile(t, scaffolded, `kind: service
name: backend
version: 0.0.0
# the agent's own note, which must survive
agent:
    publisher: codefly.dev
    kind: codefly:service
    name: go
    version: 0.0.70
spec:
    source-dir: .
endpoints:
    - name: grpc
      visibility: private
`)
	withSolutionFlags(t, "obin/wiki", nil)
	if err := applyEntryServiceShape(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(scaffolded)
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	if !strings.Contains(text, "the agent's own note") {
		t.Errorf("the agent's comments were lost:\n%s", text)
	}
	if !strings.Contains(text, "module-identity: true") {
		t.Errorf("the backend does not present the solution's identity:\n%s", text)
	}
	// An `api:` nobody wrote says nothing and is not shipped.
	if strings.Contains(text, `api: ""`) {
		t.Errorf("an empty api key was written:\n%s", text)
	}

	service := struct {
		Endpoints    []resources.Endpoint          `yaml:"endpoints"`
		Dependencies []resources.ServiceDependency `yaml:"service-dependencies"`
		Groups       []string                      `yaml:"workspace-configuration-dependencies"`
	}{}
	if err := yaml.Unmarshal(updated, &service); err != nil {
		t.Fatal(err)
	}
	// The agent's own port is replaced, not kept beside the solution's: one
	// service, one endpoint, and no surface the cell did not account for.
	if len(service.Endpoints) != 1 || service.Endpoints[0].Name != "http" ||
		service.Endpoints[0].Visibility != resources.VisibilityPublic ||
		service.Endpoints[0].Exposure != resources.ExposureNone {
		t.Fatalf("endpoints = %+v, want one public http endpoint exposed nowhere", service.Endpoints)
	}
	for _, dependency := range service.Dependencies {
		if dependency.Module != "" && dependency.Module != "authority" {
			t.Errorf("the entry service declares a direct path to %s/%s, past the authority's check",
				dependency.Module, dependency.Name)
		}
	}
	if len(service.Groups) == 0 {
		t.Error("the entry service reads no configuration group, so it cannot find the authority")
	}
}

// service-entry is load-bearing, not cosmetic: pkg/solutionrun resolves a
// module's solution manifest only for the service named by service-entry, so a
// solution whose entry does not name its backend boots with no api.consumes
// projection and every route it consumes stays unrouted.
func TestTheModuleInterfaceAndEntryNameTheScaffoldedServices(t *testing.T) {
	moduleInterface := solutionModuleInterface()
	if len(moduleInterface.Endpoints) != 2 {
		t.Fatalf("interface = %+v, want the backend and the shell", moduleInterface.Endpoints)
	}
	byService := map[string]string{}
	for _, endpoint := range moduleInterface.Endpoints {
		byService[endpoint.Service] = endpoint.Visibility
	}
	if byService[solutionEntryService] != resources.VisibilityPublic {
		t.Errorf("the backend is exported at %q, want public", byService[solutionEntryService])
	}
	// The shell is reached only by the edge that serves it, never beyond.
	if byService[solutionPageService] != resources.VisibilityInternal {
		t.Errorf("the shell is exported at %q, want internal", byService[solutionPageService])
	}
}

func TestSolutionCommandIsRegisteredAndHelpStandsAlone(t *testing.T) {
	if SolutionCmd.RunE == nil || SolutionCmd.Run != nil {
		t.Fatal("solution must use RunE so the root owns exit codes")
	}
	for name, text := range map[string]string{
		"Short": SolutionCmd.Short, "Long": SolutionCmd.Long, "Example": SolutionCmd.Example,
	} {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty; help must stand alone without network access", name)
		}
	}
	for _, flag := range []string{"package-id", "agent", "consumes", "executor", "minimum-codefly-version"} {
		if SolutionCmd.Flags().Lookup(flag) == nil {
			t.Errorf("--%s is not registered", flag)
		}
	}
	if err := SolutionCmd.Args(SolutionCmd, nil); err == nil {
		t.Error("solution accepted no name")
	}
	if err := SolutionCmd.Args(SolutionCmd, []string{"wiki"}); err != nil {
		t.Errorf("solution rejected a valid name: %v", err)
	}
}
