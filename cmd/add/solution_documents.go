package add

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/codefly-dev/cli/pkg/cli/yamledit"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/standards"
	"gopkg.in/yaml.v3"
)

// A solution has ONE shape. It is the one obin-ai/core-solutions' `wiki` has
// been running on, not the smaller one the demos carry, and every solution is
// conformed to it rather than a second being offered beside it.
//
// Two Codefly services and one page:
//
//   - backend — the entry service. It presents the solution's identity
//     (module-identity), serves the API the page calls, and is the workload a
//     host installs. It is NOT optional: it is what the solution is published
//     as, and what the principal in module.contract.codefly.yaml belongs to.
//   - web — the shell that serves the page where no host shell mounts it. The
//     same ./Page a saas host federates, in a shell of its own, which an
//     authority's edge serves as the browser's one origin. The PACKAGE does
//     not publish it: a host that federates the remote needs no shell.
//   - services/fe-remote — the ./Page remote itself. It is an npm package, not
//     a Codefly service, so this command does not scaffold it.
//
// THE ONE RULE THE BACKEND LIVES UNDER, which is not a second shape: a module
// call the authority holds to a PERSON — one its artifact interface admits
// only "in person, with no intervening actor" — is made by the page, with that
// person's own Work Context, never relayed through the backend. A backend
// reaches a module on a context minted for the solution's own audience, so
// relaying such a call puts the solution between the person and the module and
// admission refuses it. A backend that does not relay those calls is no
// intervening actor, and wiki is the proof: it keeps its backend and reaches
// robin, tracing and annotations from the page.
const (
	solutionEntryService = "backend"
	solutionPageService  = "web"
	solutionPageAgent    = "nextjs"
	// solutionEntryAgent scaffolds the backend when --agent names no other.
	// It is a SOLUTION agent, which is a different thing from the service
	// agent of the same language — see solutionAgentFor.
	solutionEntryAgent = "solution-go"
)

// serviceAgentSolutionAgent maps a SERVICE agent to the SOLUTION agent of the
// same language.
//
// The two are not interchangeable and the difference is not a detail. A
// service agent scaffolds a service: `go` writes a net/http mux serving
// /health. A solution agent scaffolds a solution: the solution runtime, the
// viewer's Work Context, a published policy on every route the authority
// checks against, the passthrough that relays a module call AS THE VIEWER,
// the interface document, and the ./Page remote a host federates.
//
// `codefly add solution` used to take the service agent and scaffold a
// service into the shape of a solution. What came out served nothing a viewer
// could reach, published no policy, and failed delivery outright ("declares a
// frontend and holds no npm project to build it from"). Nothing said so: the
// documents were all valid.
//
// So this is a REFUSAL, not a substitution. Passing `--agent go` is a
// statement about which agent to run, and quietly running a different one
// would be the CLI deciding what the author meant. It names the solution
// agent for that language instead and stops.
var serviceAgentSolutionAgent = map[string]string{
	"go":             "solution-go",
	"python":         "solution-python",
	"python-fastapi": "solution-python",
}

// solutionAgentFor checks the agent the author named is one that scaffolds a
// solution, and says which one they meant when it is not.
//
// An agent this table does not know is accepted: a publisher may ship a
// solution agent this build has never heard of, and refusing by allow-list
// would make the CLI the gatekeeper of a registry it does not own. What is
// refused is the specific, silent mistake — naming the service agent of a
// language whose solution agent exists.
func solutionAgentFor(agent string) (string, error) {
	if agent == "" {
		return solutionEntryAgent, nil
	}
	if solutionAgent, isServiceAgent := serviceAgentSolutionAgent[agent]; isServiceAgent {
		return "", fmt.Errorf(
			"--agent %s is the SERVICE agent: it scaffolds a service, not a solution — no declared operation, no published policy, no page remote, and delivery refuses the result. Pass --agent %s",
			agent, solutionAgent)
	}
	return agent, nil
}

// authorityInterfaceRange is the published interface a solution's backend
// requires: the authority gateway on a laptop, the host in a cell. A
// composition that names none is refused at boot.
//
// It is the one value here with no derivation behind it — core publishes no
// "current authority range" for a scaffold to read — so it is the range the
// running solutions require (wiki's services/backend/service.codefly.yaml),
// and --authority-interface overrides it for a composition on another.
const authorityInterfaceRange = "codefly.dev/authority@>=0.2.0 <0.4.0"

// solutionEntryEndpoint is the endpoint the backend serves: the API behind the
// page. `exposure: none` makes it reachable inside the cell and addressed from
// nowhere outside it — what the outside reaches is the host's own door.
func solutionEntryEndpoint() resources.Endpoint {
	return resources.Endpoint{
		Name:       standards.HTTP,
		Visibility: resources.VisibilityPublic,
		Exposure:   resources.ExposureNone,
		API:        standards.HTTP,
		Health: &resources.Health{Readiness: &resources.Probe{
			Kind: standards.HTTP, Path: "/health", Statuses: []string{"200"},
		}},
	}
}

// solutionPageEndpoint is the shell's endpoint. The edge serves it as the
// browser's origin and nothing outside the composition reaches it, so it
// carries no API: it answers a browser, not a contract.
func solutionPageEndpoint() resources.Endpoint {
	return resources.Endpoint{
		Name:       standards.HTTP,
		Visibility: resources.VisibilityPublic,
		Exposure:   resources.ExposureNone,
	}
}

// solutionModuleInterface is what the composition may reach across the
// solution's boundary: the backend's entry endpoint, which an edge fronts, and
// the shell, reached only by the edge that serves it. An endpoint the
// interface does not export admits no other module, so an edge could not be
// routed to one that is missing here.
func solutionModuleInterface() *resources.ModuleInterface {
	return &resources.ModuleInterface{Endpoints: []*resources.InterfaceEndpoint{
		{Service: solutionEntryService, Endpoint: standards.HTTP, Visibility: resources.VisibilityPublic},
		{Service: solutionPageService, Endpoint: standards.HTTP, Visibility: resources.VisibilityInternal},
	}}
}

// solutionEntryDependencies is the ONLY service-dependency set a solution's
// backend declares, and it is the host's own door: the authority interface,
// which the workspace binds to the one authority in scope, and the authority's
// hop, which lets through only a call the authority checked.
//
// Every consumed module is reached THROUGH that hop and gets no entry here. A
// service-dependency on a module renders into a mesh allow rule straight to
// that module's service, past the check — which is what core-solutions'
// check-solution-mesh gate refuses, and what cmd/sync/solution_sdk.go's
// missingServiceDependencies already declines to add.
//
// The hop is `kind: external`: composed by the workspace that composes this
// solution and never started from here, so it consumes no endpoint and derives
// no mesh allow rule of its own.
func solutionEntryDependencies(authorityInterface string) []authoredDependency {
	return []authoredDependency{
		{Interface: authorityInterface},
		{
			Name:      "hop",
			Module:    "authority",
			Kind:      resources.DependencyKindExternal,
			Endpoints: []map[string]string{{"name": standards.GRPC}},
		},
	}
}

// authoredDependency is the half of a service-dependency an author writes.
// core's resources.ServiceDependency carries fields a renderer fills, and its
// EndpointReference renders an empty `api:` that says nothing, so the authored
// half is spelled here — the same shape, and for the same reason, as
// cmd/sync/solution_sdk.go's applyServiceDependencies, which is the other
// place this repository appends one.
type authoredDependency struct {
	Interface string                   `yaml:"interface,omitempty"`
	Name      string                   `yaml:"name,omitempty"`
	Module    string                   `yaml:"module,omitempty"`
	Kind      resources.DependencyKind `yaml:"kind,omitempty"`
	Endpoints []map[string]string      `yaml:"endpoints,omitempty"`
}

// parseConsumedAPI reads one --consumes value into the api.consumes entry core
// specifies:
//
//	[<id>=]<protocol>:<module>/<service>/<endpoint>
//
// The id defaults to the producing module's name, and `as` — the route name
// the solution addresses it by — defaults to the id. core-solutions'
// check-consumed-apis gate requires all three of id, as and protocol on every
// entry, so none is left for core's defaults to supply.
//
// Entries carry NO scopes and no audience, deliberately. A solution declares
// its authority ONCE, in module.contract.codefly.yaml, and the CLI refuses a
// module that asks for authority from both a contract and scoped api.consumes
// (pkg/gitops/authority.go: "a module asks for authority from one
// declaration"). Unscoped entries beside a contract are accepted, so these are
// route names and nothing more.
func parseConsumedAPI(value string) (manifest.APIDeclaration, error) {
	declaration := manifest.APIDeclaration{}
	spec := strings.TrimSpace(value)
	if id, rest, found := strings.Cut(spec, "="); found {
		declaration.ID = strings.TrimSpace(id)
		spec = strings.TrimSpace(rest)
	}
	protocol, binding, found := strings.Cut(spec, ":")
	if !found {
		return declaration, fmt.Errorf("--consumes %q must be [<id>=]<protocol>:<module>/<service>/<endpoint>", value)
	}
	declaration.Protocol = strings.TrimSpace(protocol)
	parts := strings.Split(strings.TrimSpace(binding), "/")
	if len(parts) != 3 {
		return declaration, fmt.Errorf("--consumes %q names %d of module/service/endpoint; a consumed API is bound by all three or by none", value, len(parts))
	}
	declaration.Module, declaration.Service, declaration.Endpoint = parts[0], parts[1], parts[2]
	if declaration.ID == "" {
		declaration.ID = declaration.Module
	}
	declaration.As = declaration.ID
	return declaration, nil
}

// solutionManifest builds the manifest the solution ships. The schema is
// core's (solution/manifest), so this chooses VALUES and never restates the
// document: a field core adds arrives here by recompiling.
func solutionManifest(consumes []manifest.APIDeclaration, executor *resources.Agent) *manifest.Manifest {
	built := &manifest.Manifest{
		SchemaVersion:   manifest.SchemaVersionV0,
		ProtocolVersion: manifest.ProtocolVersionV0,
		// The schema calls a Service "one service the solution scaffolds", so
		// the manifest names what was scaffolded rather than leaving a reader
		// to guess. The shell is one of them: the package does not publish it,
		// but the solution does scaffold it.
		Services: []manifest.Service{
			{ID: solutionEntryService, Name: solutionEntryService},
			{ID: solutionPageService, Name: solutionPageService},
		},
		API: manifest.API{
			Exposes:  []manifest.APIDeclaration{{ID: standards.HTTP, Protocol: standards.HTTP}},
			Consumes: consumes,
		},
		// `create` and `update` stay false: they are executor operations, and
		// declaring one a packaged runtime cannot perform is what
		// Manifest.AdmitInformation refuses. This command IS the create — the
		// CLI composing verbs it already has — so nothing advertises it.
		Lifecycle: manifest.Lifecycle{Package: true, Render: true},
	}
	if executor != nil {
		built.Agent = *executor
	}
	return built
}

// validationExecutor stands in for the executor while a manifest with none is
// validated. It mirrors core's own consumptionValidationPrincipal: a field a
// later caller supplies is stood in for so every rule NOT about it is still
// checked, rather than the whole document going unvalidated.
//
// Standing one in is necessary because Manifest.Validate requires an agent of
// kind codefly:solution at a concrete semantic version and no such executor is
// published anywhere — core/solution/ holds a manifest schema and a policy and
// no executor protocol for an agent to implement (codefly-dev/cli#1215).
// Naming one that does not exist would be worse than naming none: the shipped
// manifests that name codefly.dev/solution-generic@0.0.1 name a repository
// with no releases, which resolves to "agent: binary not found" at the first
// command that tries to run it.
var validationExecutor = resources.Agent{
	Kind:      resources.SolutionAgent,
	Publisher: "codefly.dev",
	Name:      "solution",
	Version:   "0.0.0",
}

// validateSolutionManifest holds the manifest to core's rules — every rule,
// for a manifest naming an executor, and every rule but the executor's own for
// one that does not.
func validateSolutionManifest(built *manifest.Manifest) error {
	probe := *built
	if probe.Agent.Kind == "" {
		probe.Agent = validationExecutor
	}
	return probe.Validate()
}

// noExecutorPreamble is written above a manifest that names none, so the file
// says on its own why — the next author reads the file, not this source.
const noExecutorPreamble = `# No executor is named. core's schema (solution/manifest, Validate) requires an
# agent of kind codefly:solution, but core specifies no executor PROTOCOL for one
# to implement and none is published: codefly.dev/solution-generic, which the
# shipped manifests name, is a repository with no releases, so naming it resolves
# to "agent: binary not found" at the first command that runs it (cli#1215).
#
# Nothing refuses this file for it. Every reader of a shipped manifest reads the
# consumption and leaves the rest unread — core's LoadConsumption, whose own doc
# says a manifest whose agent block does not pass Load "asks for no authority
# because of it". ` + "`codefly add solution --executor`" + ` writes one the day an
# executor exists; until then every other rule of the schema was checked at
# generation against a stand-in.
`

// marshalSolutionManifest renders the manifest, dropping the agent key when no
// executor is named rather than emitting the zero Agent's four empty strings.
// The key is removed from the rendered document instead of the document being
// rebuilt from a local struct, so core's Manifest stays the only statement of
// the schema.
func marshalSolutionManifest(built *manifest.Manifest) ([]byte, error) {
	if err := validateSolutionManifest(built); err != nil {
		return nil, err
	}
	rendered, err := yaml.Marshal(built)
	if err != nil {
		return nil, err
	}
	preamble := ""
	if built.Agent.Kind == "" {
		// Drop the key rather than emit the zero Agent's four empty strings.
		if rendered, err = dropTopLevelKeys(rendered, func(key string, _ *yaml.Node) bool {
			return key == "agent"
		}); err != nil {
			return nil, err
		}
		preamble = noExecutorPreamble
	}
	// Read the BYTES back through core's own consumption reader — the one
	// every render, sync and authority derivation uses. Validating the value
	// and shipping the file is not the same check: this is the file as those
	// readers will see it.
	content := []byte(preamble + string(rendered))
	if _, err := manifest.LoadConsumption(content); err != nil {
		return nil, fmt.Errorf("the generated %s is not readable by core's consumption reader: %w", manifest.FileName, err)
	}
	return content, nil
}

// contractPreamble says what the contract is, in the file, because a reader
// meets the document before this source.
const contractPreamble = `# What this solution asks for (codefly/module-contract/v1). A request, never a
# grant. Its credentials are issued to the principal below, the id its
# module-identity services present. This is the ONE declaration of the
# solution's authority: the CLI renders the signed authority document from
# these bindings, and refuses a module that asks for authority from both a
# contract and SCOPED api.consumes — which is why solution.codefly.yaml's
# api.consumes carries route names only.
#
# It asks for nothing at another module yet. Add a binding per module call the
# solution makes ON A PERSON'S BEHALF, with the audience as a slot into the
# composition's own group: the audience a module verifies is the composition's
# to say. A call the authority holds to a person IN PERSON takes no binding
# here — the page makes it, as the person.
#
# ROLES. A solution is a unit a person installs, and the authority refuses to
# admit one that offers a person no role to hold on it ("declares 0 role(s)
# and none is \"installer\""). The installer role below is scoped to this
# solution's own page, the one resource a scaffold knows it has. Name the
# solution's real resource kinds in scope_ceilings and widen the role to them,
# and add a role per other kind of person who uses it (wiki: editor, viewer;
# proposals: reviewer, viewer). A role may grant a scope only within a
# ceiling, or through a binding it names, so the two cannot drift apart.
`

// solutionContract is the module contract the solution ships: the principal
// every credential it mints is issued to, owner-aware checking, and lists
// declared EMPTY rather than omitted — an absent list and "there are none"
// must not look the same.
//
// owner_aware is TRUE and is not a choice. The authority issues a person a
// capability at a solution only when the solution declares owner-aware
// checking (core contracts/module: "Solutions must declare it to receive
// person capabilities from the authority"), and refuses it by name otherwise.
// A solution scaffolded without it receives no owner-selected scope and can
// act for nobody.
// readAction and writeAction are the two actions a scaffolded solution's own
// vocabulary carries, spelled once: they appear in the ceiling and in each
// role that sits within it, and a typo between them is a role the authority
// silently never issues.
const (
	readAction  = "read"
	writeAction = "write"
)

func solutionContract(name string) *modulecontract.Contract {
	// The solution's own permission vocabulary, and the one role the host
	// requires. A solution is a unit a PERSON installs, so the authority
	// refuses to admit one that offers a person no role to hold on it:
	//
	//	state: forbidden: <name> declares 0 role(s) and none is "installer":
	//	a solution declares an installer role
	//
	// That refusal happens at `runtime-run`, after the whole composition has
	// booted, and every document reads fine until then — which is how seven
	// of the nine solutions in one fleet came to declare no role at all. Only
	// the two that are actually run, wiki and proposals, declare one.
	//
	// The kind is derived, not guessed at: every solution this command
	// scaffolds exposes a page, so `<name>.page` is the one resource it is
	// known to have. A solution whose domain object is something else renames
	// it here and in the role together — core's Contract.Validate refuses a
	// role granting a scope no ceiling covers, so the pair cannot drift
	// apart silently.
	ownScope := []modulecontract.CeilingScope{{
		ResourceKind: name + ".page",
		Actions:      []string{readAction, writeAction},
	}}
	return &modulecontract.Contract{
		Schema:     modulecontract.SchemaV1,
		Principal:  name,
		OwnerAware: true,
		Namespaces: []string{},
		Queues:     []string{},
		ScopeCeilings: []modulecontract.ScopeCeiling{{
			ResourceKind: name + ".page",
			Actions:      []string{readAction, writeAction},
		}},
		// installer / editor / viewer, the standard set — the same three wiki
		// declares, and now every solution in the fleet.
		//
		// `installer` is required BY NAME: the authority refuses a solution
		// without it. `viewer` is what the organization is installed as, so
		// the people of an installation can open the solution at all —
		// without a second role only the owner holds anything and everyone
		// else is refused by name. `editor` is the write position between
		// them, so a solution that grows one does not have to invent a name
		// for it; proposals calls its middle role `reviewer`, which is the
		// same position in its own domain's word.
		//
		// Every role must grant something: core refuses `role "installer"
		// grants nothing`.
		Roles: []modulecontract.Role{
			{Name: modulecontract.InstallerRole, Scopes: ownScope},
			{Name: "editor", Scopes: ownScope},
			{Name: "viewer", Scopes: []modulecontract.CeilingScope{{
				ResourceKind: name + ".page",
				Actions:      []string{readAction},
			}}},
		},
		Bindings:     []modulecontract.Binding{},
		Destinations: []modulecontract.Destination{},
	}
}

// solutionFrontend is the page's declaration, read by core's own strict
// parser. `./Page` is the Module Federation remote a host federates, which is
// why the shell and the host mount the same module.
func solutionFrontend(title string, order int64) *solutionhost.Frontend {
	return &solutionhost.Frontend{
		Title:         title,
		Order:         order,
		ExposedModule: "./Page",
		HostContract:  1,
	}
}

// marshalSolutionFrontend renders the frontend declaration and reads it back
// through core's strict parser — the same reader that will refuse it at
// delivery, so a file that would fail there fails here instead.
func marshalSolutionFrontend(frontend *solutionhost.Frontend) ([]byte, error) {
	rendered, err := yaml.Marshal(struct {
		Title         string `yaml:"title"`
		Order         int64  `yaml:"order"`
		ExposedModule string `yaml:"exposed_module"`
		HostContract  uint64 `yaml:"host_contract"`
	}{frontend.Title, frontend.Order, frontend.ExposedModule, frontend.HostContract})
	if err != nil {
		return nil, err
	}
	if _, err := solutionhost.ParseFrontend(rendered); err != nil {
		return nil, fmt.Errorf("the generated %s is invalid: %w", solutionrun.FrontendFileName, err)
	}
	return rendered, nil
}

// packagePreamble states what the package identity is for, in the file.
const packagePreamble = `# The solution's package identity. A solution is delivered to a host the way a
# module is: the render declares its presence from this file, and a solution
# without one declares no presence, so the host never lists it and nobody can
# install it. Authority is effective from that presence generation too, so the
# scopes module.contract.codefly.yaml declares reach no host without it.
#
# The id is the identity the solution is published under; the version is the
# package's, not the repository tag's: it moves when what the package publishes
# moves. The shell service is deliberately absent below — a host that federates
# the page's remote needs no shell of its own.
`

// solutionPackage is the package identity the solution ships.
//
// Every value but the id is derived. The contract ranges come from core's
// DefaultSupportedContracts, so the day core supports a new major the scaffold
// writes that one; the Codefly floor comes from the build doing the
// scaffolding. The id is the one thing no derivation reaches: it is the
// identity the solution is PUBLISHED under, owned by whoever publishes it, and
// a guess here would be committed and wrong.
func solutionPackage(id, toolVersion, declaredFloor string) (*composition.PackageManifest, error) {
	contracts := map[string]string{}
	for contract, supported := range composition.DefaultSupportedContracts {
		if contract != composition.ContractComposition && contract != composition.ContractFrontendPlugin {
			continue
		}
		major, err := majorRange(supported)
		if err != nil {
			return nil, fmt.Errorf("cannot derive the %s contract range: %w", contract, err)
		}
		contracts[contract] = major
	}
	floor := declaredFloor
	if floor == "" {
		derived, err := codeflyFloor(toolVersion)
		if err != nil {
			return nil, err
		}
		floor = derived
	}
	return &composition.PackageManifest{
		Kind:                  composition.PackageKind,
		Schema:                composition.PackageSchema,
		ID:                    id,
		Version:               "0.1.0",
		MinimumCodeflyVersion: floor,
		// "contracts" alongside "services": `codefly generate contracts`
		// writes the backend's published http contract to
		// contracts/api/backend/http/openapi.json, and a package that does
		// not declare the root cannot carry it — "the package cannot carry
		// the backend/http contract ... add \"contracts\" under
		// artifact-roots", which is where the generate stops.
		//
		// It is not a packaging detail. The CLI derives the authority's
		// per-route method policies from the contract this package carries
		// (ServiceMethodPolicies reads api-contracts), so without it the
		// authority is delivered the module's ceilings and no method policy
		// at all — "no delivered method policies; declared ceilings alone do
		// not admit a call". The backend serves its route, the route declares
		// its policy, and every call is still refused, with the only sign a
		// warning in the authority's log.
		ArtifactRoots: []string{"contracts", "services"},
		// The backend only. The shell is scaffolded and composed; it is not
		// published, because a host that federates the remote needs no shell.
		Services:  []composition.ProvidedService{{Name: solutionEntryService, Endpoints: []string{standards.HTTP}}},
		Contracts: contracts,
	}, nil
}

// majorRange turns the contract versions core supports into the range a
// package declares: the newest supported major, open to the end of that major.
func majorRange(supported []string) (string, error) {
	best := -1
	for _, value := range supported {
		major, _, _ := strings.Cut(value, ".")
		parsed, err := strconv.Atoi(major)
		if err != nil {
			return "", fmt.Errorf("supported version %q has no major", value)
		}
		if parsed > best {
			best = parsed
		}
	}
	if best < 0 {
		return "", fmt.Errorf("core supports no version of this contract")
	}
	return fmt.Sprintf(">=%d.0.0 <%d.0.0", best, best+1), nil
}

// codeflyFloor is the Codefly requirement a freshly scaffolded package
// declares: the build that scaffolded it, open to the end of its major. A
// package cannot claim to work with a Codefly older than the one that wrote
// its documents, and the upper bound is the major it was written against.
//
// A build from source reports no release version ("development"), and there is
// no honest floor to derive from it — the package would claim compatibility
// with a release that may not exist yet. That is a case for the author to
// state, not for this to guess, so it is refused by name with the flag that
// settles it.
func codeflyFloor(toolVersion string) (string, error) {
	parsed, err := semver.NewVersion(strings.TrimPrefix(toolVersion, "v"))
	if err != nil {
		return "", fmt.Errorf("this Codefly reports version %q, which is not a release, so the floor the package declares cannot be derived from it: pass --minimum-codefly-version (e.g. \">=0.1.220 <1.0.0\")", toolVersion)
	}
	return fmt.Sprintf(">=%d.%d.%d <%d.0.0", parsed.Major(), parsed.Minor(), parsed.Patch(), parsed.Major()+1), nil
}

// dropTopLevelKeys re-renders a freshly marshalled document without the
// top-level keys drop selects. It exists so a document can keep core's own
// struct as the one statement of its schema — a local struct with different
// tags would be a second statement that drifts — while still not shipping a
// key whose value says nothing.
func dropTopLevelKeys(rendered []byte, drop func(key string, value *yaml.Node) bool) ([]byte, error) {
	document, root, err := yamledit.Document(rendered)
	if err != nil {
		return nil, err
	}
	content := make([]*yaml.Node, 0, len(root.Content))
	for index := 0; index+1 < len(root.Content); index += 2 {
		if drop(root.Content[index].Value, root.Content[index+1]) {
			continue
		}
		content = append(content, root.Content[index], root.Content[index+1])
	}
	root.Content = content
	return yamledit.Marshal(document)
}

// isNullNode reports a YAML null — the rendering of a nil pointer field that
// no omitempty tag suppresses.
func isNullNode(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!null"
}
