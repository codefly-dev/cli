// Package solutionshape holds the one shape a solution has, as a check a
// module is judged against.
//
// It exists because "the right shape" was, until now, checkable only against
// somebody's survey of the solutions that happened to exist — which is how
// obin-ai/core-solutions reached nine solutions in four different states, two
// of them unable to receive a person capability at all and nobody able to see
// it. A shape nothing can check is a convention, and a convention drifts.
//
// Every rule here is one that silently broke something. None is a style
// preference, and a rule whose breach only looks untidy does not belong here.
package solutionshape

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/standards"
)

// SchemaVersion is the report's version, so a consumer that parses --json can
// tell what it is reading.
const SchemaVersion = "codefly.solution-shape-report/v1"

// The services the shape names. The entry service is the workload a host
// installs; the page service is the shell that serves the page where no host
// shell mounts it.
const (
	EntryService = "backend"
	PageService  = "web"
	// FrontendFileName is the page declaration, beside the ENTRY service.
	// pkg/solutionrun reads it from there and nowhere else.
	FrontendFileName = "solution.frontend.codefly.yaml"
)

// Status is a check's outcome.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Stable diagnostic codes. A caller scripts against these, so a code is never
// renamed or reused for a different meaning — it is retired and a new one
// added.
const (
	CodeNotASolution            = "not_a_solution"
	CodeManifestUnreadable      = "solution_manifest_unreadable"
	CodeConsumedAPIUnnamed      = "consumed_api_unnamed"
	CodeServiceEntryMissing     = "service_entry_missing"
	CodeServiceEntryNotTheEntry = "service_entry_not_the_entry"
	CodeEntryServiceMissing     = "entry_service_missing"
	CodeEntryServiceUnreadable  = "entry_service_unreadable"
	CodeModuleIdentityMissing   = "module_identity_missing"
	CodeModuleEdgeDeclared      = "module_edge_declared"
	CodeEntryEndpointMissing    = "entry_endpoint_missing"
	CodeContractMissing         = "module_contract_missing"
	CodeContractUnreadable      = "module_contract_unreadable"
	CodeOwnerAwareMissing       = "owner_aware_missing"
	CodePackageMissing          = "module_package_missing"
	CodePackageUnreadable       = "module_package_unreadable"
	CodePackagePublishesShell   = "module_package_publishes_shell"
	CodeFrontendMissing         = "frontend_declaration_missing"
	CodeFrontendUnreadable      = "frontend_declaration_unreadable"
	CodePageServiceMissing      = "page_shell_missing"
	CodeInterfaceMissing        = "module_interface_missing"
	CodeEntryNotExported        = "entry_endpoint_not_exported"
	CodeConsumedWithoutBinding  = "consumed_module_without_binding"
	CodePageShellHasNoApp       = "page_shell_has_no_app"
	CodePageExportedTooWidely   = "page_shell_exported_too_widely"
	CodeInstallerRoleMissing    = "installer_role_missing"
)

// Check is one rule's outcome, with the fix when it failed.
type Check struct {
	Code        string `json:"code"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// Report is what one module was judged to be.
type Report struct {
	SchemaVersion string  `json:"schema_version"`
	Module        string  `json:"module"`
	Dir           string  `json:"dir"`
	IsSolution    bool    `json:"is_solution"`
	Status        Status  `json:"status"`
	Checks        []Check `json:"checks"`
}

// Conforms reports whether nothing failed. A warning is not a failure: it
// names something worth seeing that does not make the solution wrong.
func (r *Report) Conforms() bool { return r.Status != StatusFail }

// Inspect judges the module at dir against the shape. It is strictly
// read-only: it opens files and writes none.
//
// A directory with no solution manifest is not a solution, and is reported as
// such rather than as a failing one — this command is run across a tree of
// modules, and a plain module is not a broken solution.
func Inspect(moduleName, dir string) *Report {
	report := &Report{SchemaVersion: SchemaVersion, Module: moduleName, Dir: dir, Status: StatusOK}

	manifestPath := filepath.Join(dir, manifest.FileName)
	raw, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		report.add(Check{Code: CodeNotASolution, Status: StatusOK,
			Message: fmt.Sprintf("no %s: this module is not a solution", manifest.FileName)})
		return report
	}
	report.IsSolution = true
	if err != nil {
		report.fail(CodeManifestUnreadable, fmt.Sprintf("cannot read %s: %v", manifest.FileName, err),
			"the file must be readable for anything else to be judged")
		return report
	}

	consumed := report.checkManifest(raw)
	module := report.checkModule(dir)
	report.checkEntryService(dir)
	report.checkContract(dir, consumed)
	report.checkPackage(dir)
	report.checkFrontend(dir)
	if module != nil {
		report.checkInterface(module)
	}
	return report
}

// checkManifest holds the solution manifest to the reader every render, sync
// and authority derivation actually uses, then to the one rule those readers
// do not enforce: a consumed entry with no route name.
func (r *Report) checkManifest(raw []byte) []string {
	loaded, err := manifest.LoadConsumption(raw)
	if err != nil {
		r.fail(CodeManifestUnreadable,
			fmt.Sprintf("%s is not readable by core's consumption reader: %v", manifest.FileName, err),
			"every render, sync and authority derivation reads the manifest this way, so nothing can resolve what this solution consumes")
		return nil
	}
	r.ok(CodeManifestUnreadable, fmt.Sprintf("%s declares %d consumed binding(s)", manifest.FileName, len(loaded.API.Consumes)))

	// `as` is the route name the solution addresses a module by. core's schema
	// lets it default; the runtime projection and the consumer repository's
	// own gate both require it, so an entry without one resolves to nothing.
	for index := range loaded.API.Consumes {
		consumed := &loaded.API.Consumes[index]
		if consumed.As == "" {
			r.fail(CodeConsumedAPIUnnamed,
				fmt.Sprintf("api.consumes entry %q names no route (`as`)", consumed.ID),
				"give it `as: <name>`; the runtime addresses the module by that name and an entry without one is unroutable")
			return nil
		}
	}
	if len(loaded.API.Consumes) > 0 {
		r.ok(CodeConsumedAPIUnnamed, "every consumed API names its route")
	}
	seen := map[string]bool{}
	var modules []string
	for index := range loaded.API.Consumes {
		if module := loaded.API.Consumes[index].Module; module != "" && !seen[module] {
			seen[module] = true
			modules = append(modules, module)
		}
	}
	return modules
}

// checkModule holds the module manifest's service-entry, which is load-bearing
// and silent when wrong.
func (r *Report) checkModule(dir string) *resources.Module {
	module, err := resources.LoadModuleFromDir(context.Background(), dir)
	if err != nil {
		r.fail(CodeServiceEntryMissing, fmt.Sprintf("cannot read %s: %v", resources.ModuleConfigurationName, err),
			"the module manifest must be readable")
		return nil
	}
	switch module.ServiceEntry {
	case "":
		r.fail(CodeServiceEntryMissing, "the module declares no service-entry",
			fmt.Sprintf("set `service-entry: %s`; a run resolves the solution manifest only through the entry, so without it the api.consumes projection is never built and every consumed route stays unrouted", EntryService))
	case EntryService:
		r.ok(CodeServiceEntryMissing, "service-entry names the backend")
	default:
		r.fail(CodeServiceEntryNotTheEntry,
			fmt.Sprintf("service-entry is %q, not %q", module.ServiceEntry, EntryService),
			"a run resolves the solution manifest only for the service named here, so the api.consumes projection is built for the wrong service or not at all")
	}

	if hasService(module, PageService) {
		r.ok(CodePageServiceMissing, "the page's shell is declared")
		r.checkPageShellHasAnApp(dir)
	} else {
		r.warn(CodePageServiceMissing,
			fmt.Sprintf("the module declares no %q service", PageService),
			"the shell serves the page where no host shell mounts the remote; a solution that is only ever federated by a host does not need one, but every other composition does")
	}
	return module
}

// checkEntryService holds the backend to the two things that are the shape's
// and not the agent's: it presents the solution's identity, and it opens no
// path to a module.
func (r *Report) checkEntryService(dir string) {
	path := filepath.Join(dir, "services", EntryService, resources.ServiceConfigurationName)
	service, err := resources.LoadFromDir[resources.Service](context.Background(), filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		r.fail(CodeEntryServiceMissing, fmt.Sprintf("no %s", path),
			"the entry service is the workload a host installs; a solution manifest with no backend beside it is refused by the consumer repository's own gate")
		return
	}
	if err != nil {
		r.fail(CodeEntryServiceUnreadable, fmt.Sprintf("cannot read the %s service manifest: %v", EntryService, err), "")
		return
	}
	r.ok(CodeEntryServiceMissing, "the entry service is present")

	if service.ModuleIdentity {
		r.ok(CodeModuleIdentityMissing, "the backend presents the solution's identity")
	} else {
		r.fail(CodeModuleIdentityMissing, "the backend does not declare module-identity",
			"the authority takes this service's projected credential as the actor of every Work Context it issues the solution, and refuses an authority request from a module with no such service")
	}

	// THE MESH RULE. A solution reaches every module through the HOST'S DOOR,
	// which checks the call. A service-dependency on anything else renders
	// into an allow rule straight to that module's service, past the check.
	//
	// There are two doors, and which one a solution uses is the composition's,
	// not the solution's. Under a saas host it is that host's gateway and
	// frontend; under the host authority interface it is the authority's hop,
	// and ONLY in a manifest that also requires that interface — without it,
	// an edge to the hop is a path to a module service like any other.
	// This is the same table core-solutions' check_solution_mesh.py holds.
	interfaces := map[string]bool{}
	for _, dependency := range service.ServiceDependencies {
		if dependency != nil && dependency.Interface != "" {
			if key := interfaceKey(dependency.Interface); key != "" {
				interfaces[key] = true
			}
		}
	}
	offending := 0
	for _, dependency := range service.ServiceDependencies {
		if dependency == nil || dependency.Module == "" {
			continue
		}
		if isHostDoor(dependency.Module, dependency.Name, interfaces) {
			continue
		}
		offending++
		r.fail(CodeModuleEdgeDeclared,
			fmt.Sprintf("the backend declares a direct path to %s/%s", dependency.Module, dependency.Name),
			"a solution reaches a module only through the host's door — the saas gateway and frontend, or the authority's hop beside the authority interface. This edge renders into a mesh allow rule past the authority's check")
	}
	if offending == 0 {
		r.ok(CodeModuleEdgeDeclared, "the backend opens no direct path to a module")
	}

	if endpointNamed(service, standards.HTTP) {
		r.ok(CodeEntryEndpointMissing, "the backend serves its http endpoint")
	} else {
		r.fail(CodeEntryEndpointMissing, "the backend declares no http endpoint",
			"the page and the API behind it are served on it, and the module interface exports it")
	}
}

// checkContract holds the contract to the declaration that fails silently.
func (r *Report) checkContract(dir string, consumed []string) {
	if _, err := os.Stat(filepath.Join(dir, modulecontract.FileName)); errors.Is(err, os.ErrNotExist) {
		r.fail(CodeContractMissing, fmt.Sprintf("no %s", modulecontract.FileName),
			"it declares the principal every credential the solution mints is issued to; without it the solution has no declared identity to issue against")
		return
	}
	contract, err := modulecontract.Load(dir)
	if err != nil {
		r.fail(CodeContractUnreadable, fmt.Sprintf("cannot read %s: %v", modulecontract.FileName, err),
			"note that a core older than v0.19.7 refuses the WHOLE contract when it carries owner_aware, because core holds the document to its known fields")
		return
	}
	r.ok(CodeContractMissing, fmt.Sprintf("the contract declares principal %q", contract.Principal))
	r.checkConsumedModulesHaveAuthority(consumed, contract)

	r.checkInstallerRole(contract)

	// The one that fails silently, and the reason this checker exists.
	if contract.OwnerAware {
		r.ok(CodeOwnerAwareMissing, "the contract declares owner-aware checking")
	} else {
		r.fail(CodeOwnerAwareMissing, "the contract does not declare owner-aware checking",
			"the authority issues a person a capability at a solution ONLY when it declares owner_aware: true, and refuses by name otherwise. Without it this solution receives no owner-selected scope and can act for nobody — and nothing fails at build time to tell you")
	}
}

// checkInstallerRole holds the contract to the role the host requires of a
// solution. A solution is a unit a PERSON installs, so the authority refuses
// to admit one that offers a person no role to hold on it:
//
//	state: forbidden: <name> declares 0 role(s) and none is "installer":
//	a solution declares an installer role
//
// It is reported here because of WHEN that refusal arrives: at `runtime-run`,
// after the whole composition — authority, store, every consumed module — has
// booted. Every document reads fine until then, `manifest.Validate` passes,
// and the three consumer-repository gates pass. Of the nine solutions in one
// fleet, seven declared no role at all and only the two that are actually
// run, wiki and proposals, declared one; the shape checker called all nine
// conformant.
//
// Only the role's EXISTENCE is checked. What it should grant is the
// solution's own business, and core's Contract.Validate already refuses a
// role granting a scope no ceiling covers or no binding spells out — a rule
// this package must not restate, having once shipped a permissions check that
// failed wiki and proposals, the only two solutions known to work.
func (r *Report) checkInstallerRole(contract *modulecontract.Contract) {
	for index := range contract.Roles {
		if contract.Roles[index].Name == modulecontract.InstallerRole {
			r.ok(CodeInstallerRoleMissing, "the contract declares the installer role")
			return
		}
	}
	r.fail(CodeInstallerRoleMissing,
		fmt.Sprintf("the contract declares %d role(s) and none is %q", len(contract.Roles), modulecontract.InstallerRole),
		"the authority refuses to admit a solution that offers a person no role to hold on it, and it refuses at runtime-run — after the whole composition has booted and every document has read fine. Declare an installer role scoped within the contract's own scope_ceilings")
}

// checkPackage holds the published identity, and the one thing about it that
// is easy to get wrong in the right-looking direction.
func (r *Report) checkPackage(dir string) {
	if _, err := os.Stat(filepath.Join(dir, composition.PackageManifestFileName)); errors.Is(err, os.ErrNotExist) {
		r.fail(CodePackageMissing, fmt.Sprintf("no %s", composition.PackageManifestFileName),
			"the render declares the solution's presence from this file; without it the host never lists the solution and nobody can install it, and the contract's scopes reach no host either")
		return
	}
	packaged, err := composition.LoadPackageManifest(dir)
	if err != nil {
		r.fail(CodePackageUnreadable, fmt.Sprintf("cannot read %s: %v", composition.PackageManifestFileName, err), "")
		return
	}
	r.ok(CodePackageMissing, fmt.Sprintf("the package publishes %s@%s", packaged.ID, packaged.Version))

	for index := range packaged.Services {
		if packaged.Services[index].Name == PageService {
			r.warn(CodePackagePublishesShell,
				fmt.Sprintf("the package publishes the %q shell", PageService),
				"a host that federates the page's remote needs no shell of its own, so the shell is composed but not published")
			return
		}
	}
	r.ok(CodePackagePublishesShell, "the package does not publish the shell")
}

// checkFrontend reads the page declaration with core's own strict parser —
// the reader that refuses it at delivery — and from beside the ENTRY service,
// which is the only place anything looks.
func (r *Report) checkFrontend(dir string) {
	path := filepath.Join(dir, "services", EntryService, FrontendFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		r.warn(CodeFrontendMissing, fmt.Sprintf("no %s beside the entry service", FrontendFileName),
			"a solution with no page declaration is headless: the host lists it but mounts nothing")
		return
	}
	if err != nil {
		r.fail(CodeFrontendUnreadable, fmt.Sprintf("cannot read %s: %v", FrontendFileName, err), "")
		return
	}
	frontend, err := solutionhost.ParseFrontend(raw)
	if err != nil {
		r.fail(CodeFrontendUnreadable, fmt.Sprintf("%s is refused by core's parser: %v", FrontendFileName, err),
			"this is the same reader that refuses it at delivery, so the solution would be delivered and then fail")
		return
	}
	r.ok(CodeFrontendMissing, fmt.Sprintf("the page declares %q exposing %s", frontend.Title, frontend.ExposedModule))
}

// checkInterface holds what the composition may reach across the solution's
// boundary. An endpoint the interface does not export admits no other module,
// so an edge cannot be routed to it.
func (r *Report) checkInterface(module *resources.Module) {
	if module.Interface == nil || len(module.Interface.Endpoints) == 0 {
		r.fail(CodeInterfaceMissing, "the module exports no interface",
			fmt.Sprintf("export %s/%s at public so an edge can be routed to it, and the shell at internal", EntryService, standards.HTTP))
		return
	}
	var entry, page *resources.InterfaceEndpoint
	for _, endpoint := range module.Interface.Endpoints {
		switch endpoint.Service {
		case EntryService:
			entry = endpoint
		case PageService:
			page = endpoint
		}
	}
	switch {
	case entry == nil:
		r.fail(CodeEntryNotExported, fmt.Sprintf("the interface does not export %s/%s", EntryService, standards.HTTP),
			"an endpoint the interface does not export admits no other module, so the edge in front of the solution cannot be routed to it")
	case entry.Visibility != "" && entry.Visibility != resources.VisibilityPublic:
		r.fail(CodeEntryNotExported,
			fmt.Sprintf("the interface exports %s at %q, not public", EntryService, entry.Visibility),
			"the edge that fronts the solution reaches it from outside the module")
	default:
		r.ok(CodeEntryNotExported, "the interface exports the backend at public")
	}

	// The shell is reached by the edge that serves it and by nothing else.
	if page != nil && page.Visibility == resources.VisibilityPublic {
		r.warn(CodePageExportedTooWidely,
			fmt.Sprintf("the interface exports the %q shell at public", PageService),
			"the shell is reached only by the edge that serves it as the browser's origin; internal is the reach it needs")
	} else if page != nil {
		r.ok(CodePageExportedTooWidely, "the interface exports the shell at internal")
	}
}

func (r *Report) add(check Check) {
	r.Checks = append(r.Checks, check)
	if check.Status == StatusFail {
		r.Status = StatusFail
	}
}

func (r *Report) ok(code, message string) {
	r.add(Check{Code: code, Status: StatusOK, Message: message})
}

func (r *Report) warn(code, message, remediation string) {
	r.add(Check{Code: code, Status: StatusWarn, Message: message, Remediation: remediation})
}

func (r *Report) fail(code, message, remediation string) {
	r.add(Check{Code: code, Status: StatusFail, Message: message, Remediation: remediation})
}

func hasService(module *resources.Module, name string) bool {
	for _, reference := range module.ServiceReferences {
		if reference != nil && reference.Name == name {
			return true
		}
	}
	return false
}

func endpointNamed(service *resources.Service, name string) bool {
	for _, endpoint := range service.Endpoints {
		if endpoint != nil && endpoint.Name == name {
			return true
		}
	}
	return false
}

// hostDoors are the (module, service) pairs a solution may hold an edge to
// unconditionally: a saas host's own gateway and frontend.
var hostDoors = map[[2]string]bool{
	{"saas", "auth-gateway"}: true,
	{"saas", "frontend"}:     true,
}

// authorityDoors are the (module, service) pairs that are a door ONLY in a
// manifest that also requires the named interface. Without it, an edge to the
// hop is a path to a module service like any other.
var authorityDoors = map[[2]string]string{
	{"authority", "hop"}: "codefly.dev/authority",
}

// isHostDoor reports whether an edge is the host's own door rather than a path
// past it.
func isHostDoor(module, name string, interfaces map[string]bool) bool {
	if hostDoors[[2]string{module, name}] {
		return true
	}
	if required, ok := authorityDoors[[2]string{module, name}]; ok {
		return interfaces[required]
	}
	return false
}

// interfaceKey is the <publisher>/<name> of an interface requirement, which
// core spells <publisher>/<name>@<range>, or "" when it is not one.
func interfaceKey(reference string) string {
	key, _, found := strings.Cut(reference, "@")
	if !found || strings.Count(key, "/") != 1 {
		return ""
	}
	publisher, name, _ := strings.Cut(key, "/")
	if publisher == "" || name == "" {
		return ""
	}
	return key
}

// WHAT IS DELIBERATELY NOT CHECKED HERE, and why it would be wrong to try.
//
// "Can a person holding role R actually call module M?" is the
// missing-permission question, and it cannot be answered from these documents
// alone. A role reaches another module's authority two ways: by naming a
// binding in role.Bindings, and by declaring that module's resource kinds in
// role.Scopes — which is how wiki and proposals do it, granting `documents`
// and `documents.collection` with no RoleBinding at all. A binding's own kinds
// are composition SLOTS, so which kinds it covers is not known until the
// composition supplies values.
//
// A first draft of this package failed any binding no role named by id. It
// reported wiki and proposals — the two solutions that have been running
// longest — as broken, which is how the second mechanism was found. The real
// answer needs modulecontract.Contract.Resolve(values) against a composition's
// configuration, which is a composition-scoped check and not a
// directory-scoped one.

// checkConsumedModulesHaveAuthority reports a module the solution consumes for
// which the contract declares no binding. This is NOT always wrong — a module
// reached with the viewer's own credential, or on nobody's behalf, asks for no
// binding — which is why it warns rather than fails. It is reported because
// the two declarations live in different files, so nothing else puts them side
// by side, and "the binding was never added" and "this one needs none" look
// identical until a call is refused.
func (r *Report) checkConsumedModulesHaveAuthority(consumes []string, contract *modulecontract.Contract) {
	if len(consumes) == 0 {
		return
	}
	bound := map[string]bool{}
	for i := range contract.Bindings {
		for _, audience := range contract.Bindings[i].Calls {
			for _, call := range audience {
				bound[call] = true
			}
		}
		bound[contract.Bindings[i].ID] = true
	}
	var unbound []string
	for _, module := range consumes {
		if !bound[module] {
			unbound = append(unbound, module)
		}
	}
	if len(unbound) == 0 {
		r.ok(CodeConsumedWithoutBinding, "every consumed module has a binding")
		return
	}
	r.warn(CodeConsumedWithoutBinding,
		fmt.Sprintf("consumed with no binding in the contract: %s", strings.Join(unbound, ", ")),
		"correct when the module is reached with the VIEWER'S own credential or on nobody's behalf; a bug when the solution calls it on a person's behalf, and then the call is refused at runtime with nothing failing earlier. Confirm each one")
}

// checkPageShellHasAnApp reports a shell that is declared and has no app
// behind it — a service.codefly.yaml naming a Next.js service with no
// package.json beside it.
//
// It is a real state and not a hypothetical: conforming the fleet to one shape
// added the shell's MANIFEST to several solutions whose page had never been
// written, because writing one would have been inventing product in a
// conforming change. The declaration is right and the gap is real, so
// `codefly run service web` has nothing to start on those.
//
// A warning, not a failure: the solution is correctly shaped, and nothing is
// broken until someone runs the shell. It is reported because a declared
// service with no source is otherwise invisible — every manifest reads fine,
// and the first sign is a run that does nothing.
func (r *Report) checkPageShellHasAnApp(dir string) {
	shell := filepath.Join(dir, "services", PageService)
	service, err := resources.LoadFromDir[resources.Service](context.Background(), shell)
	if err != nil {
		// No manifest at all is the module declaring a service it does not
		// have, which the entry-service rules already speak to.
		return
	}
	app := filepath.Join(shell, pageSourceDir(service))
	for _, marker := range []string{"package.json", "next.config.ts", "next.config.js"} {
		if _, statErr := os.Stat(filepath.Join(app, marker)); statErr == nil {
			r.ok(CodePageShellHasNoApp, "the page's shell has an app behind it")
			return
		}
	}
	r.warn(CodePageShellHasNoApp,
		fmt.Sprintf("the %q shell is declared with no app behind it", PageService),
		"`codefly run service "+PageService+"` has nothing to start. Write the shell, or drop the service from module.codefly.yaml — a declaration with no source is the one state neither reading the manifests nor running the gates reveals")
}

// pageSourceDir is where the shell's app lives, relative to the service: the
// manifest's `spec.source-dir`, or `code` when it says nothing.
//
// The empty default is the nextjs agent's own (`Settings.NodeSourceDir`), and
// the two layouts in use differ by exactly this key: a shell this CLI
// scaffolds takes the agent's default and gets an app under `code/`, while the
// fleet's hand-written pages sit at the service root and say `source-dir: .`.
// A check that assumed either one called the other empty — the scaffolded
// shell was reported as having no app while its package.json sat one directory
// down.
func pageSourceDir(service *resources.Service) string {
	if dir, ok := service.Spec["source-dir"].(string); ok && dir != "" {
		return dir
	}
	return "code"
}
