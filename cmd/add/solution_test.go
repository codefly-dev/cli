package add

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// TestTheServiceAgentOfALanguageIsRefused. `codefly add solution --agent go`
// used to scaffold a Go SERVICE into the shape of a solution: a net/http mux
// serving /health, no declared operation, no published policy and no page
// remote. Every document was valid, nothing said a word, and delivery refused
// the result for a frontend it had no npm project to build.
//
// The refusal names the solution agent rather than substituting it: which
// agent to run is the author's statement, and quietly running another is the
// CLI deciding what they meant.
func TestTheServiceAgentOfALanguageIsRefused(t *testing.T) {
	for serviceAgent, solutionAgent := range map[string]string{
		"go":             "solution-go",
		"python":         "solution-python",
		"python-fastapi": "solution-python",
	} {
		resolved, err := solutionAgentFor(serviceAgent)
		if err == nil {
			t.Fatalf("--agent %s was accepted and resolved to %q; the service agent scaffolds a service, not a solution", serviceAgent, resolved)
		}
		if !strings.Contains(err.Error(), solutionAgent) {
			t.Fatalf("refusing --agent %s must name %s to use instead, got: %v", serviceAgent, solutionAgent, err)
		}
	}
}

// TestTheDefaultAndAnUnknownAgentAreBothAccepted. An agent this build has
// never heard of is accepted: a publisher may ship a solution agent newer
// than this CLI, and refusing by allow-list would make the CLI the gatekeeper
// of a registry it does not own.
func TestTheDefaultAndAnUnknownAgentAreBothAccepted(t *testing.T) {
	resolved, err := solutionAgentFor("")
	if err != nil || resolved != solutionEntryAgent {
		t.Fatalf("no --agent must resolve to %s, got %q (%v)", solutionEntryAgent, resolved, err)
	}
	if resolved, err = solutionAgentFor("solution-rust"); err != nil || resolved != "solution-rust" {
		t.Fatalf("an unknown agent must be accepted as named, got %q (%v)", resolved, err)
	}
}

// TestTheShellLinksAndMountsTheRemote. The shell the nextjs agent leaves is a
// standalone web app: placeholder login, signup and dashboard routes and
// "Your app is running. Edit src/app/page.tsx to get started." Nothing in it
// is a solution, and nothing else writes it — the solution agent runs for the
// BACKEND and the nextjs agent runs after it, so anything the agent wrote
// there is overwritten. This command is the only step after both.
func TestTheShellLinksAndMountsTheRemote(t *testing.T) {
	manifest, err := addJSONDependency([]byte(`{
  "name": "code",
  "dependencies": { "next": "16.4.1" }
}`), "notes-fe-remote", "file:../../fe-remote")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = json.Unmarshal(manifest, &document); err != nil {
		t.Fatal(err)
	}
	dependencies, _ := document["dependencies"].(map[string]any)
	if dependencies["notes-fe-remote"] != "file:../../fe-remote" {
		t.Fatalf("the remote must be linked into the shell, got %v", dependencies)
	}
	if dependencies["next"] != "16.4.1" {
		t.Fatalf("linking the remote must not drop the shell's own dependencies, got %v", dependencies)
	}

	// The shell mounts the remote's ./Page with ssr: false — the same module
	// a host federates, imported rather than reimplemented.
	app := shellApp("notes", "notes-fe-remote")
	for _, required := range []string{`import("notes-fe-remote/Page")`, "ssr: false", `const apiBase = "/v1/notes"`} {
		if !strings.Contains(app, required) {
			t.Fatalf("the shell must contain %q", required)
		}
	}

	// Turbopack refuses to follow the remote's symlink out of the app's own
	// root, so the build root is the solution's. Without it the shell answers
	// 500 while the link, the exports map and the file are all correct.
	config := shellNextConfigSource("notes-fe-remote")
	for _, required := range []string{`root: resolve("../../..")`, `transpilePackages: ["notes-fe-remote"]`, "externalDir: true"} {
		if !strings.Contains(config, required) {
			t.Fatalf("the shell's next.config.ts must contain %q", required)
		}
	}
	// wiki's `webpack` block is a hard error on the Next this agent
	// scaffolds: Next 16 runs Turbopack and refuses a webpack config.
	if strings.Contains(config, "webpack(") {
		t.Fatal("a webpack config is refused by the Next this agent scaffolds")
	}
}

// TestTheLocalRunNamesTheEdge. The edge is the browser's one origin: it
// signs the person in and routes /v1/<name> to the backend with their Work
// Context. It is NOT a dependency of the backend and nothing elects it as
// ingress, so neither `run solution` nor `run workspace` starts it — the
// fleet's own Makefiles name it first, and so does this one.
//
// Getting it wrong does not look like an error. Open the shell's own port and
// the page says "This page is not behind its sign-in edge"; open the
// authority's REST port and every path 404s. Both were hit repeatedly
// reconstructing this by hand, which is why it is scaffolded.
func TestTheLocalRunNamesTheEdge(t *testing.T) {
	makefile := localMakefile("notes")
	if !strings.Contains(makefile, "SERVICES = authority/edge notes/web notes/backend") {
		t.Fatalf("the edge must be named first in the services the playground runs:\n%s", makefile)
	}

	// The edge group is what tells the edge which module to route and where
	// the page is. Its key is the solution's name, upper-cased.
	group := localEdgeGroup("my-notes")
	for _, required := range []string{"MODULES=my-notes", "MY_NOTES_REST=${endpoint:my-notes/backend/http}", "FRONTEND=${endpoint:my-notes/web/http}"} {
		if !strings.Contains(group, required) {
			t.Fatalf("the edge group must contain %q, got:\n%s", required, group)
		}
	}

	// The authority is pinned by commit, and the pin must be one the DEFAULT
	// BRANCH holds: a PR head reads identically and dies on squash-merge.
	workspace := localWorkspace("notes", "88599779642808bce196d612b99b0898be6b78c3")
	for _, required := range []string{`version: "88599779642808bce196d612b99b0898be6b78c3"`, "source: codefly-dev/authority", "fixture: local", "name: notes-local", "path: ."} {
		if !strings.Contains(workspace, required) {
			t.Fatalf("the playground workspace must contain %q", required)
		}
	}
}

// TestTheInstallationAdmitsTheFixturesPeople. A solution the authority has no
// installation for refuses every capability — "audience %q is not installed
// in this organization, so there is no installation to seal the capability
// to" — so the page can only render its not-installed branch. The fixture
// itself names who exists; which solutions are installed is each composed
// module's fragment to say.
//
// TEAMS ARE WHAT ADMIT ANYONE BUT THE OWNER, and dropping them is the
// mistake this guards: with `role` and `owner` alone every other person of
// the fixture signs in and is refused by name, "<person> holds no role on
// <solution>". Verified against the running authority — maya, rémi, alex and
// dev all reach the solution's route; no-roles, in no team, gets 403.
func TestTheInstallationAdmitsTheFixturesPeople(t *testing.T) {
	fragment := fixtureComposition("notes")
	for _, required := range []string{
		"schema: codefly.dev/authority/fixture-composition/v1",
		"organization: acme",
		"target: notes",
		"owner: alex",
		"teams: [finance, engineering]",
	} {
		if !strings.Contains(fragment, required) {
			t.Fatalf("the fixture composition must contain %q, got:\n%s", required, fragment)
		}
	}
}

// TestTheContractDeclaresARoleBesidesInstaller. The organization is installed
// as viewer, so the contract has to declare that role: a role the contract
// does not name is one the authority never issues.
func TestTheContractDeclaresARoleBesidesInstaller(t *testing.T) {
	contract := solutionContract("notes")
	var names []string
	for index := range contract.Roles {
		names = append(names, contract.Roles[index].Name)
	}
	if !slices.Equal(names, []string{"installer", "editor", "viewer"}) {
		t.Fatalf("the contract must declare the standard set installer/editor/viewer, got %v", names)
	}
}

// TestTheLocalRunBuildsTheRemoteFirst. The solution runtime serves /assets/
// from ASSETS_DIR, which defaults to ../fe-remote/dist relative to the
// backend. With nothing built there the backend answers 404 for
// /assets/mf-manifest.json — the manifest it advertises in
// /.well-known/solution.json — and a host can federate nothing. The fleet's
// own playground builds the remote before the run for exactly this reason.
func TestTheLocalRunBuildsTheRemoteFirst(t *testing.T) {
	makefile := localMakefile("notes")
	if !strings.Contains(makefile, "run: remote") {
		t.Fatalf("the local run must build the remote first:\n%s", makefile)
	}
	for _, required := range []string{
		"cd services/fe-remote && npm install",
		"npm run build",
		"test -f services/fe-remote/dist/mf-manifest.json",
	} {
		if !strings.Contains(makefile, required) {
			t.Fatalf("the remote target must contain %q", required)
		}
	}
}

// TestTheLocalRunRefusesWithoutTheRegistryToken. The page's kits are
// published to GitHub Packages and the remote's .npmrc reads
// NODE_AUTH_TOKEN, so without it npm answers "401 Unauthorized -
// unauthenticated" and names no cause. The playground says so itself rather
// than letting a newcomer meet that.
func TestTheLocalRunRefusesWithoutTheRegistryToken(t *testing.T) {
	makefile := localMakefile("notes")
	if !strings.Contains(makefile, "NODE_AUTH_TOKEN") {
		t.Fatalf("the remote target must check for the registry token:\n%s", makefile)
	}
	if !strings.Contains(makefile, "gh auth token") {
		t.Fatal("the refusal must name the command that sets it")
	}
	if !strings.Contains(localReadme("notes"), "NODE_AUTH_TOKEN") {
		t.Fatal("the local run doc must state the token as a prerequisite")
	}
}

// TestTheSolutionDirectoryIsTheWorkspace. The local composition sits at the
// solution's own root, as it does in all nine solutions of the fleet —
// core-solutions has no root workspace of its own, so each solution IS one.
//
// It is deliberately NOT a playground/ subdirectory: a playground is a
// MODULE's, somewhere to exercise a module on its own, and two solutions
// carry one as an extra. Scaffolding into one also made the module path ".."
// instead of ".", which resolved the module a level up and failed the run on
// a module.codefly.yaml that was not there.
func TestTheSolutionDirectoryIsTheWorkspace(t *testing.T) {
	workspace := localWorkspace("notes", "88599779642808bce196d612b99b0898be6b78c3")
	if !strings.Contains(workspace, "path: .\n") {
		t.Fatalf("the module path must be \".\", the workspace sitting beside module.codefly.yaml:\n%s", workspace)
	}
	if strings.Contains(workspace, "path: ..") {
		t.Fatal("\"..\" is the path a separate playground directory needed; the workspace is at the solution root")
	}
	if strings.Contains(localMakefile("notes"), "cd playground") {
		t.Fatal("the run happens at the solution root, not in a playground subdirectory")
	}
}
