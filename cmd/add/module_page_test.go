package add

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/cli/pkg/solutionshape"
	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/solutionhost"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

// withModulePageFlags sets the command's flag state for one test and restores it.
func withModulePageFlags(t *testing.T, packageID string) {
	t.Helper()
	previous := struct {
		packageID, title, minimum string
		order                     int64
	}{modulePagePackageID, modulePageTitle, modulePageMinimumCodefly, modulePageOrder}
	t.Cleanup(func() {
		modulePagePackageID, modulePageTitle, modulePageMinimumCodefly, modulePageOrder =
			previous.packageID, previous.title, previous.minimum, previous.order
	})
	modulePagePackageID, modulePageTitle, modulePageOrder = packageID, "", 50
	// A test binary reports no release version.
	modulePageMinimumCodefly = ">=0.1.230 <1.0.0"
}

func modulePageDocumentsByName(t *testing.T, name string) map[string][]byte {
	t.Helper()
	withModulePageFlags(t, "acme/"+name)
	rendered, err := modulePageDocuments(name, name)
	if err != nil {
		t.Fatalf("modulePageDocuments: %v", err)
	}
	out := map[string][]byte{}
	for _, document := range rendered {
		out[document.name] = document.content
	}
	return out
}

func scopeStrings(scopes []modulecontract.CeilingScope) []string {
	var out []string
	for _, scope := range scopes {
		for _, action := range scope.Actions {
			out = append(out, scope.ResourceKind+":"+action)
		}
	}
	return out
}

// Each role grants exactly what the operations its holders call require: the
// viewer reads, the editor reads and writes, and the installer reads and
// configures the page — it does not write entries it has no reason to write.
func TestModulePageRolesGrantOnlyWhatTheirOperationsRequire(t *testing.T) {
	contract := modulePageContract("notes", "notes")
	if err := contract.Validate(); err != nil {
		t.Fatalf("core refuses the contract: %v", err)
	}
	want := map[string][]string{
		modulecontract.InstallerRole: {"notes.entry:read", "notes.page:configure"},
		editorRole:                   {"notes.entry:read", "notes.entry:write"},
		viewerRole:                   {"notes.entry:read"},
	}
	if len(contract.Roles) != len(want) {
		t.Fatalf("roles = %+v, want installer, editor and viewer", contract.Roles)
	}
	for _, role := range contract.Roles {
		if got := scopeStrings(role.Scopes); !slices.Equal(got, want[role.Name]) {
			t.Errorf("%s grants %v, want %v", role.Name, got, want[role.Name])
		}
	}
	if contract.InstallationRole != "none" {
		t.Errorf("installation_role = %q: a module page holds no standing authority", contract.InstallationRole)
	}
	if !contract.RequireMethodPolicies {
		t.Error("a published method with no policy is not refused before delivery")
	}
}

// The contract names, above each scope a role grants, the operation that
// needs it, and the file with those comments is still the contract core reads.
func TestModulePageContractNamesTheOperationBesideEachScope(t *testing.T) {
	content := modulePageDocumentsByName(t, "notes")[modulecontract.FileName]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, modulecontract.FileName), content, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := modulecontract.Load(dir)
	if err != nil {
		t.Fatalf("core cannot read the commented contract: %v\n%s", err, content)
	}
	if !slices.EqualFunc(loaded.Roles, modulePageContract("notes", "notes").Roles, func(a, b modulecontract.Role) bool {
		return a.Name == b.Name && slices.Equal(scopeStrings(a.Scopes), scopeStrings(b.Scopes))
	}) {
		t.Errorf("the comments changed what the roles grant: %+v", loaded.Roles)
	}
	text := string(content)
	for _, line := range []string{
		"# notes.entry:read: ListEntries (",
		"# notes.entry:write: PutEntry (",
		"# notes.page:configure: ConfigurePage (",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("no %q beside its scope:\n%s", line, text)
		}
	}
}

// Every method the proto declares is marked, and with exactly the scope the
// contract's roles are derived from.
func TestModulePageProtoMarksEveryMethodWithItsPolicy(t *testing.T) {
	source := modulePageProto("notes", "notes", pageProtoService("notes"))
	if !strings.Contains(source, `import "`+hopPolicyProtoPath+`";`) || !strings.Contains(source, "service NotesService {") {
		t.Fatalf("the proto does not import the policy option or name its service:\n%s", source)
	}
	if got, want := strings.Count(source, "rpc "), len(pageOperations); got != want {
		t.Fatalf("%d rpcs, want %d", got, want)
	}
	if got := strings.Count(source, "option (authority.hop.v1.policy)"); got != len(pageOperations) {
		t.Errorf("%d of %d methods carry a policy", got, len(pageOperations))
	}
	for _, line := range []string{
		`option (authority.hop.v1.policy) = {any_checked_caller: true};`,
		`option (authority.hop.v1.policy) = {requires: [{resource_kind: "notes.entry", actions: ["read"]}]};`,
		`option (authority.hop.v1.policy) = {requires: [{resource_kind: "notes.entry", actions: ["write"]}]};`,
		`option (authority.hop.v1.policy) = {requires: [{resource_kind: "notes.page", actions: ["configure"]}]};`,
	} {
		if !strings.Contains(source, line) {
			t.Errorf("no %s", line)
		}
	}
}

// pageDescriptorSet is the FileDescriptorSet buf builds from the scaffold's
// proto, built here from the same operations: the published contract the
// doctor and the authority read the policies from.
func pageDescriptorSet(t *testing.T, module string) []byte {
	t.Helper()
	api := &descriptorpb.FileDescriptorProto{
		Name: proto.String("api.proto"), Package: proto.String("api"), Syntax: proto.String("proto3"),
		Dependency: []string{hopPolicyProtoPath},
	}
	service := &descriptorpb.ServiceDescriptorProto{Name: proto.String(pageProtoService(module))}
	for _, operation := range pageOperations {
		for _, suffix := range []string{"Request", "Response"} {
			api.MessageType = append(api.MessageType, &descriptorpb.DescriptorProto{Name: proto.String(operation.method + suffix)})
		}
		policy := &hopv1.Policy{AnyCheckedCaller: operation.kind == ""}
		if operation.kind != "" {
			policy.Requires = []*hopv1.Requirement{{ResourceKind: pageKind(module, operation.kind), Actions: []string{operation.action}}}
		}
		options := &descriptorpb.MethodOptions{}
		proto.SetExtension(options, hopv1.E_Policy, policy)
		service.Method = append(service.Method, &descriptorpb.MethodDescriptorProto{
			Name:      proto.String(operation.method),
			InputType: proto.String(".api." + operation.method + "Request"), OutputType: proto.String(".api." + operation.method + "Response"),
			Options: options,
		})
	}
	api.Service = []*descriptorpb.ServiceDescriptorProto{service}
	set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(descriptorpb.File_google_protobuf_descriptor_proto),
		protodesc.ToFileDescriptorProto(hopv1.File_authority_hop_v1_policy_proto),
		api,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// writeModulePage lays the scaffold's documents out as a module directory,
// with the published contract its package names.
func writeModulePage(t *testing.T, module string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(relative string, content []byte) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range modulePageDocumentsByName(t, module) {
		write(name, content)
	}
	interfaceYAML, err := yaml.Marshal(modulePageInterface(module))
	if err != nil {
		t.Fatal(err)
	}
	write("module.codefly.yaml", []byte("kind: module\nname: "+module+"\nservice-entry: "+module+"\nservices:\n    - name: "+module+"\ninterface:\n"+indent(string(interfaceYAML))))

	descriptor := pageDescriptorSet(t, module)
	path := "contracts/api/" + module + "/connect/contract.binpb"
	write(path, descriptor)
	sum := sha256.Sum256(descriptor)
	packaged, err := composition.LoadPackageManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	packaged.Services[0].APIContracts = []composition.ProvidedAPIContract{{
		Endpoint: "connect", Kind: composition.APIContractKindProtobuf, Package: "api", Path: path, Digest: "sha256:" + hex.EncodeToString(sum[:]),
	}}
	content, err := yaml.Marshal(packaged)
	if err != nil {
		t.Fatal(err)
	}
	write(composition.PackageManifestFileName, content)
	return dir
}

func indent(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for index := range lines {
		lines[index] = "    " + lines[index]
	}
	return strings.Join(lines, "\n") + "\n"
}

// The doctor judges the scaffold a module page and finds nothing to warn
// about: no grant an operation does not require, no operation no role
// reaches, installation_role none. Each role reaches the operations it is
// derived from, as the authority's own admission decides.
func TestTheScaffoldedModulePagePassesTheDoctorWithNoWarning(t *testing.T) {
	report := solutionshape.Inspect("notes", writeModulePage(t, "notes"))
	if !report.IsModulePage || report.IsSolution {
		t.Fatalf("judged module page=%v solution=%v", report.IsModulePage, report.IsSolution)
	}
	for _, check := range report.Checks {
		if check.Status != solutionshape.StatusOK {
			t.Errorf("%s %s: %s", check.Status, check.Code, check.Message)
		}
	}
	want := map[string][]string{
		modulecontract.InstallerRole: {"/api.NotesService/ConfigurePage", "/api.NotesService/ListEntries"},
		editorRole:                   {"/api.NotesService/ListEntries", "/api.NotesService/PutEntry"},
		viewerRole:                   {"/api.NotesService/ListEntries"},
	}
	for _, role := range report.Roles {
		if !slices.Equal(role.Operations, want[role.Role]) {
			t.Errorf("%s reaches %v, want %v", role.Role, role.Operations, want[role.Role])
		}
	}
}

// The documents are the ones a host reads a module page from: a page
// declaration beside the entry, a package publishing the entry's Connect
// endpoint, and no solution manifest — which would make it a solution.
func TestModulePageDocumentsAreAModulePageNotASolution(t *testing.T) {
	documents := modulePageDocumentsByName(t, "notes")
	if _, found := documents["solution.codefly.yaml"]; found {
		t.Fatal("a module page ships no solution manifest")
	}
	frontend, found := documents["services/notes/"+solutionrun.FrontendFileName]
	if !found {
		t.Fatal("no page declaration beside the entry")
	}
	if parsed, err := solutionhost.ParseFrontend(frontend); err != nil || parsed.ExposedModule != "./Page" {
		t.Fatalf("page declaration %+v: %v", parsed, err)
	}
	var packaged composition.PackageManifest
	if err := yaml.Unmarshal(documents[composition.PackageManifestFileName], &packaged); err != nil {
		t.Fatal(err)
	}
	if err := packaged.Validate(); err != nil {
		t.Fatalf("package: %v", err)
	}
	if len(packaged.Services) != 1 || packaged.Services[0].Name != "notes" || !slices.Equal(packaged.Services[0].Endpoints, []string{"connect"}) {
		t.Errorf("package publishes %+v, want the entry's connect endpoint", packaged.Services)
	}
	readme := string(documents["README.md"])
	for _, phrase := range []string{"Installed by composition", "Nobody can use it until a role is granted", "Set up", "notes-viewer"} {
		if !strings.Contains(readme, phrase) {
			t.Errorf("the README does not say %q", phrase)
		}
	}
}

func TestModulePageRefusesWithoutAPublishedIdentity(t *testing.T) {
	withModulePageFlags(t, "")
	if _, err := modulePageDocuments("notes", "notes"); err == nil || !strings.Contains(err.Error(), "--package-id") {
		t.Fatalf("got %v, want a refusal naming --package-id", err)
	}
}

// The page calls the module's own procedures on the host's module edge,
// through the generated client, and never on apiBase.
func TestThePageCallsTheModuleOnTheModuleEdge(t *testing.T) {
	files := modulePageRemote("notes", "Notes", "notes-client", "../../../../libraries/notes-client/typescript")
	page := files["src/Page.tsx"]
	for _, want := range []string{
		`import { NotesService } from "notes-client";`,
		"createClient(NotesService, transport)",
		"baseUrl: edge",
		"moduleApiBase?.trim()",
		"client.listEntries({})",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Page.tsx lacks %q", want)
		}
	}
	if strings.Contains(page, "baseUrl: apiBase") {
		t.Error("the page calls the module on apiBase, the host's relay for a solution runtime")
	}
	if !strings.Contains(files["package.json"], `"notes-client": "file:../../../../libraries/notes-client/typescript"`) {
		t.Errorf("the page does not depend on its generated client:\n%s", files["package.json"])
	}
	if !strings.Contains(files["rsbuild.config.ts"], `name: "notesPage"`) {
		t.Errorf("container name:\n%s", files["rsbuild.config.ts"])
	}
	if got := remoteGlobalName("org-directory"); got != "orgDirectoryPage" {
		t.Errorf("remoteGlobalName = %q", got)
	}
}

// The vendored option proto is the one whose descriptor the CLI registers,
// so the policies the scaffold writes are read by the same extension.
func TestTheVendoredPolicyProtoIsTheRegisteredOne(t *testing.T) {
	registered := hopv1.File_authority_hop_v1_policy_proto
	text := string(hopPolicyProto)
	if registered.Path() != hopPolicyProtoPath {
		t.Errorf("registered at %s, vendored at %s", registered.Path(), hopPolicyProtoPath)
	}
	for _, want := range []string{
		"package " + string(registered.Package()) + ";",
		`option go_package = "` + hopPolicyGoPackage + `;hopv1";`,
		"Policy policy = 51100;",
		"repeated Requirement requires = 1;",
		"bool any_checked_caller = 2;",
		"string resource_kind = 1;",
		"repeated string actions = 2;",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the vendored policy.proto lacks %q", want)
		}
	}
	if field := hopv1.E_Policy.TypeDescriptor(); field.Number() != 51100 {
		t.Errorf("the registered extension is %d", field.Number())
	}
}

// The scaffold states the entry's endpoint answers and leaves every other
// question to the prompt.
func TestStatedAnswersAnswerTheirConfirmationsAndDeferTheRest(t *testing.T) {
	answers := &statedAnswers{confirms: modulePageEntryAnswers, rest: deferred{}}
	confirm := func(name string, def bool) *agentv0.Question {
		return &agentv0.Question{Message: &agentv0.Message{Name: name}, Value: &agentv0.Question_Confirm{Confirm: &agentv0.Confirm{Default: def}}}
	}
	for name, want := range map[string]bool{"rest-endpoint": false, "connect-endpoint": true} {
		answer, err := answers.Answer(context.Background(), confirm(name, !want))
		if err != nil || answer.GetConfirm().GetConfirmed() != want {
			t.Errorf("%s answered %v (%v), want %v", name, answer, err, want)
		}
	}
	if _, err := answers.Answer(context.Background(), confirm("hot-reload", true)); err != errDeferred {
		t.Errorf("an unstated question was not deferred: %v", err)
	}
}

var errDeferred = os.ErrNotExist

type deferred struct{}

func (deferred) Answer(context.Context, *agentv0.Question) (*agentv0.Answer, error) {
	return nil, errDeferred
}

func TestModulePageCommandIsRegisteredAndHelpStandsAlone(t *testing.T) {
	if ModulePageCmd.RunE == nil || ModulePageCmd.Run != nil {
		t.Fatal("module-page must use RunE so the root owns exit codes")
	}
	for name, text := range map[string]string{"Short": ModulePageCmd.Short, "Long": ModulePageCmd.Long, "Example": ModulePageCmd.Example} {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty", name)
		}
	}
	for _, flag := range []string{"package-id", "title", "order", "minimum-codefly-version", "yes"} {
		if ModulePageCmd.Flags().Lookup(flag) == nil {
			t.Errorf("--%s is not registered", flag)
		}
	}
	if err := ModulePageCmd.Args(ModulePageCmd, nil); err == nil {
		t.Error("module-page accepted no name")
	}
}
