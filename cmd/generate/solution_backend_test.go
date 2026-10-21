package generate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/standards"
)

// testdata/solution-backend/api.swagger.json is the solution runtime's own
// rendering (solution-runtime-go v0.0.15, solution.InterfaceDocument) of a
// backend declaring two documented operations — `GET /items` answering an
// itemList, `POST /items/create` reading a createItem and answering an item —
// under the title "notes solution backend". It is what a solution backend
// commits at standards.OpenAPIPath, byte for byte.
//
// The path is made absolute at package initialisation, while the working
// directory is still this package's: the tests chdir into their workspace.
var solutionBackendDocumentFixture, _ = filepath.Abs("testdata/solution-backend/api.swagger.json")

// solutionBackendRoutes are the routes that document declares.
var solutionBackendRoutes = []composition.APIContractRoute{
	{Method: "GET", Path: "/items"},
	{Method: "POST", Path: "/items/create"},
}

// A solution manifest as a solution ships it beside its module manifest; the
// CLI reads it only to know the module is a solution.
const solutionBackendManifest = `schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
api:
    exposes:
        - id: http
          protocol: http
`

// saveSolutionBackendWorkspace writes a workspace whose one module is a
// solution: it ships a solution manifest, names backend as its service-entry,
// and backend declares one public http endpoint, the shape a solution
// backend's service.codefly.yaml has. withDocument commits the interface
// document at standards.OpenAPIPath. It returns the workspace root and the
// module directory.
func saveSolutionBackendWorkspace(t *testing.T, withDocument bool) (string, string) {
	t.Helper()
	ctx := context.Background()
	root, moduleDir := saveFixtureWorkspace(t, ctx, "notes",
		&resources.ModuleInterface{
			Endpoints: []*resources.InterfaceEndpoint{
				{Service: "backend", Endpoint: "http", Visibility: resources.VisibilityPublic},
			},
		},
		&resources.Service{
			Name:    "backend",
			Version: "0.0.1",
			Endpoints: []*resources.Endpoint{
				{Name: "http", API: standards.HTTP, Visibility: resources.VisibilityPublic, Exposure: resources.ExposureNone},
			},
		},
	)
	module, err := resources.LoadModuleFromDir(ctx, moduleDir)
	if err != nil {
		t.Fatal(err)
	}
	module.ServiceEntry = "backend"
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, manifest.FileName), []byte(solutionBackendManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if withDocument {
		document, err := os.ReadFile(solutionBackendDocumentFixture)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(moduleDir, "services", "backend", standards.OpenAPIPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, document, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, moduleDir
}

func readSolutionBackendDocument(t *testing.T) []byte {
	t.Helper()
	document, err := os.ReadFile(solutionBackendDocumentFixture)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func sortedRoutes(routes []composition.APIContractRoute) []composition.APIContractRoute {
	out := slices.Clone(routes)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// A solution backend's http endpoint exports the interface document the
// backend commits as its contract: the same bytes, the document's routes, and
// api http. No Builder is loaded for it (none is installed here), so the
// export reads only the committed file.
func TestGenerateContractsExportsASolutionBackendInterfaceDocument(t *testing.T) {
	root, moduleDir := saveSolutionBackendWorkspace(t, true)
	t.Chdir(root)
	resetContractsFlags(t)
	contractsFormat = "json"

	out, err := captureStdout(t, func() error {
		return ContractsCmd.RunE(ContractsCmd, []string{"notes"})
	})
	if err != nil {
		t.Fatalf("RunE: %v\noutput:\n%s", err, out)
	}
	var catalog composition.APIContractCatalog
	if err := json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&catalog); err != nil {
		t.Fatalf("cannot parse catalog JSON: %v\noutput:\n%s", err, out)
	}
	if len(catalog.Endpoints) != 1 {
		t.Fatalf("catalog.Endpoints = %+v, want the backend's http endpoint", catalog.Endpoints)
	}
	entry := catalog.Endpoints[0]
	document := readSolutionBackendDocument(t)
	if entry.Service != "backend" || entry.Endpoint != "http" || entry.API != standards.HTTP || entry.Kind != composition.APIContractKindOpenAPI {
		t.Fatalf("entry = %+v, want backend/http, api http, kind openapi", entry)
	}
	if entry.Package != "notes solution backend" || entry.Path != "contracts/api/backend/http/openapi.json" || entry.Digest != composition.APIContractDigest(document) {
		t.Fatalf("entry = %+v, want the document's title, path and digest", entry)
	}
	if got := sortedRoutes(entry.Routes); !reflect.DeepEqual(got, solutionBackendRoutes) {
		t.Fatalf("routes = %+v, want %+v", got, solutionBackendRoutes)
	}
	written, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash(entry.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(document) {
		t.Fatal("the exported contract is not the committed interface document")
	}

	// --check reads the same document, so a fresh export is not drift.
	resetContractsFlags(t)
	contractsCheck = true
	if out, err := captureStdout(t, func() error { return ContractsCmd.RunE(ContractsCmd, []string{"notes"}) }); err != nil {
		t.Fatalf("--check: %v\noutput:\n%s", err, out)
	}
}

// A solution backend that commits no interface document has nothing to export:
// its endpoint is skipped like any contract-less export, with a notice saying
// where the document goes and what renders it.
func TestGenerateContractsSkipsASolutionBackendWithoutInterfaceDocument(t *testing.T) {
	root, _ := saveSolutionBackendWorkspace(t, false)
	t.Chdir(root)
	resetContractsFlags(t)
	contractsFormat = "json"

	out, err := captureStdout(t, func() error {
		return ContractsCmd.RunE(ContractsCmd, []string{"notes"})
	})
	if err != nil {
		t.Fatalf("RunE: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "solution backend notes/backend commits no interface document for its http endpoint at openapi/api.swagger.json") {
		t.Fatalf("output missing the backend's skip notice:\n%s", out)
	}
	var catalog composition.APIContractCatalog
	if err := json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&catalog); err != nil {
		t.Fatalf("cannot parse catalog JSON: %v\noutput:\n%s", err, out)
	}
	if len(catalog.Endpoints) != 0 {
		t.Fatalf("catalog.Endpoints = %+v, want none", catalog.Endpoints)
	}
}

// --from <solution>/backend resolves to the interface document the backend
// commits, with no Builder loaded: the source a frontend client of the
// backend's documented operations is generated from.
func TestGenerateClientResolvesASolutionBackendToItsInterfaceDocument(t *testing.T) {
	root, _ := saveSolutionBackendWorkspace(t, true)
	t.Chdir(root)
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	document := readSolutionBackendDocument(t)

	for _, from := range []string{"notes/backend", "notes/backend/http"} {
		source, err := resolveClientSource(ctx, workspace, from, "")
		if err != nil {
			t.Fatalf("%s: %v", from, err)
		}
		if source.endpoint.Kind != composition.APIContractKindOpenAPI || source.endpoint.API != standards.HTTP || source.endpoint.Endpoint != "http" {
			t.Fatalf("%s: endpoint = %+v, want the backend's http endpoint with an openapi contract", from, source.endpoint)
		}
		if string(source.contractBytes) != string(document) || source.endpoint.Digest != composition.APIContractDigest(document) {
			t.Fatalf("%s: the contract is not the committed interface document", from)
		}
		if got := sortedRoutes(source.endpoint.Routes); !reflect.DeepEqual(got, solutionBackendRoutes) {
			t.Fatalf("%s: routes = %+v, want %+v", from, got, solutionBackendRoutes)
		}
		if source.packageID != "local" || source.packageVersion != "0.0.1" || source.moduleName != "notes" {
			t.Fatalf("%s: source identity = %s@%s (%s)", from, source.packageID, source.packageVersion, source.moduleName)
		}
	}
}

// Without a committed interface document the request is refused with what to
// commit, instead of "has no grpc or rest endpoint".
func TestGenerateClientRefusesASolutionBackendWithoutInterfaceDocument(t *testing.T) {
	root, _ := saveSolutionBackendWorkspace(t, false)
	t.Chdir(root)
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolveClientSource(ctx, workspace, "notes/backend", "")
	if err == nil || !strings.Contains(err.Error(), "solution backend notes/backend commits no interface document for its http endpoint at openapi/api.swagger.json") {
		t.Fatalf("err = %v, want the missing-document refusal", err)
	}
}

// Only a solution's entry service is its backend. The same http service and
// document in a module that ships no solution manifest is not one: its http
// endpoint keeps having no contract.
func TestSolutionBackendRequiresASolutionEntryService(t *testing.T) {
	ctx := context.Background()
	_, moduleDir := saveSolutionBackendWorkspace(t, true)
	module, err := resources.LoadModuleFromDir(ctx, moduleDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := module.LoadServiceFromName(ctx, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if backend, err := solutionBackendOf(ctx, module, service); err != nil || backend == nil || backend.document == nil {
		t.Fatalf("the solution's entry service: backend = %+v, err = %v", backend, err)
	}

	if err := os.Remove(filepath.Join(moduleDir, manifest.FileName)); err != nil {
		t.Fatal(err)
	}
	if backend, err := solutionBackendOf(ctx, module, service); err != nil || backend != nil {
		t.Fatalf("a module with no solution manifest: backend = %+v, err = %v, want none", backend, err)
	}
}

// Every OpenAPI contract records each route at its own path. Core gives a
// route its full path and keys its group by that same path, so a join of the
// two recorded "/items" as "/items/items".
func TestOpenAPIContractRoutesAreTheDocumentPaths(t *testing.T) {
	path := solutionBackendDocumentFixture
	rest, err := resources.LoadRestAPI(context.Background(), &path)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := openAPIContractEndpoint(&basev0.Endpoint{Name: "rest", Service: "api", Api: standards.REST}, rest, "openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedRoutes(entry.Routes); !reflect.DeepEqual(got, solutionBackendRoutes) {
		t.Fatalf("routes = %+v, want %+v", got, solutionBackendRoutes)
	}
}

// End to end through the proto companion: the frontend client of a solution
// backend's documented operations is generated from its interface document.
// The operations' request and response types land under src/, the directory
// the package builds, and the barrel re-exports them, so importing the package
// reaches them.
func TestGenerateClientFromASolutionBackendTypeScript(t *testing.T) {
	requireQualify(t)
	root, _ := saveSolutionBackendWorkspace(t, true)
	t.Chdir(root)
	resetClientFlags(t)
	clientFrom = "notes/backend"
	clientLanguages = []string{"typescript"}
	clientOutput = filepath.Join(t.TempDir(), "lib")

	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	assertLibraryLayout(t, clientOutput, "typescript")

	typescript := filepath.Join(clientOutput, "typescript")
	types := filepath.Join(typescript, "src", "gen", "api.ts")
	for _, want := range []string{`"/items": {`, `"/items/create": {`, "ItemList: {", "CreateItem: {"} {
		assertFileContains(t, types, want)
	}
	assertFileContains(t, filepath.Join(typescript, "src", "index.ts"), `export * from "./gen/api.js";`)
	for _, stale := range []string{filepath.Join(typescript, "gen"), filepath.Join(typescript, "src", "gen", "api.d.ts")} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("%s exists (%v): the types belong in src/gen/api.ts only", stale, err)
		}
	}
}
