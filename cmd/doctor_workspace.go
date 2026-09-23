package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
	hostprovider "github.com/codefly-dev/cli/pkg/provider"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/tui"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Stable diagnostic codes for workspace readiness checks. Automation (worktree
// managers, CI) matches on these — never on message prose. Adding a code is
// fine; renaming or removing one is a breaking change to the JSON contract and
// requires bumping doctorWorkspaceSchemaVersion.
const (
	codeWorkspaceNotFound          = "workspace_not_found"
	codeWorkspaceInvalid           = "workspace_invalid"
	codeEnvironmentNotFound        = "environment_not_found"
	codeServiceNotFound            = "service_not_found"
	codeModuleNotFound             = "module_not_found"
	codeConfigurationDirMissing    = "configuration_directory_missing"
	codeConfigurationMissing       = "configuration_missing"
	codeConfigurationInvalid       = "configuration_invalid"
	codeConfigurationDuplicate     = "configuration_duplicate"
	codeProviderNotConfigured      = "provider_not_configured"
	codeProviderExecutableMissing  = "provider_executable_missing"
	codeProviderAuthRequired       = "provider_authentication_required"
	codeProviderResolutionFailed   = "provider_resolution_failed"
	codePlaintextNotAllowed        = "plaintext_not_allowed"
	codeReferenceSchemeUnknown     = "reference_scheme_unknown"
	codeModuleReferenceUnresolved  = "module_reference_unresolved"
	codeModuleTrustMissing         = "module_trust_missing"
	codeModuleUnverified           = "module_unverified"
	codeModuleResolutionGit        = "module_resolution_git"
	codeModuleResolutionStale      = "module_resolution_stale"
	codeModuleNotMaterialized      = "module_not_materialized"
	codeModuleCheckoutVersionDrift = "module_checkout_version_drift"
	codeServiceOverrideActive      = "service_override_active"
	codeServiceOverrideUnresolved  = "service_override_unresolved"
	codeServiceOverrideDrift       = "service_override_contract_drift"
	codeTimeout                    = "timeout"
)

const doctorWorkspaceSchemaVersion = 1

const (
	readinessStatusReady    = "ready"
	readinessStatusNotReady = "not_ready"
)

// Per-check statuses, as they appear in the JSON report.
const (
	checkStatusFail = "fail"
)

type workspaceDiagnostic struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Status      string `json:"status"` // ok | warn | fail
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type workspaceReadinessReport struct {
	SchemaVersion       int                   `json:"schema_version"`
	Workspace           string                `json:"workspace,omitempty"`
	WorkspaceDir        string                `json:"workspace_dir,omitempty"`
	Environment         string                `json:"environment"`
	EnvironmentDeclared bool                  `json:"environment_declared"`
	Module              string                `json:"module,omitempty"`
	Service             string                `json:"service,omitempty"`
	Status              string                `json:"status"`
	Checks              []workspaceDiagnostic `json:"checks"`
}

func (report *workspaceReadinessReport) add(code, name, status, message, remediation string) {
	report.Checks = append(report.Checks, workspaceDiagnostic{
		Code: code, Name: name, Status: status, Message: message, Remediation: remediation,
	})
	if status == checkStatusFail {
		report.Status = readinessStatusNotReady
	}
}

type workspaceReadinessOptions struct {
	// dir pins the workspace directory (tests). Empty means the usual upward
	// discovery from the current directory.
	dir string
	env string
	// module narrows the scope to the services of one module, for a verb that
	// acts on exactly one — `deploy gitops render <module>`. A sibling module's
	// missing configuration is then not this module's problem.
	module  string
	service string
	timeout time.Duration
}

// workspaceReadiness runs the read-only readiness validation and always
// returns a report; every failure is a diagnostic, never a bare error. It
// must not create, modify, or delete any file, and must not start agents,
// containers, or services. Secret references are resolved in memory through
// the environment's configured backend and the values discarded immediately.
func workspaceReadiness(ctx context.Context, opts workspaceReadinessOptions) *workspaceReadinessReport {
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	report := &workspaceReadinessReport{
		SchemaVersion: doctorWorkspaceSchemaVersion,
		Environment:   opts.env,
		Status:        readinessStatusReady,
	}

	ws := checkWorkspace(ctx, opts, report)
	if ws == nil {
		return report
	}

	checkReferencedModules(ctx, ws, report)
	checkVendoredPins(ctx, ws, report)
	checkModuleTrust(ctx, ws, report)
	checkServiceOverrides(ctx, ws, report)
	materialized := checkModulesMaterialized(ctx, ws, report)

	env := checkEnvironment(ws, opts.env, report)
	if env == nil {
		return report
	}

	checkProviderBindings(ctx, ws, env, report)

	resolvers, unavailable := checkSecretProviders(env, report)

	// Everything from here loads the workspace's services, which core refuses
	// for a composed module nobody has materialized on this machine. The
	// diagnostic above already names each such module and the command that
	// pulls it; relaying core's refusal on top would report the same condition
	// a second time, worded as a manifest error.
	if !materialized {
		return report
	}

	scope, requiredBy := checkScope(ctx, ws, opts.module, opts.service, report)
	if scope == nil {
		return report
	}

	toResolve := checkConfigurationSources(ctx, ws, env, opts.module != "" || opts.service != "", scope, requiredBy, report)

	checkSecretReferences(ctx, env, resolvers, unavailable, toResolve, report)

	return report
}

func checkWorkspace(ctx context.Context, opts workspaceReadinessOptions, report *workspaceReadinessReport) *resources.Workspace {
	dir := opts.dir
	if dir == "" {
		found, err := resources.FindUp[resources.Workspace](ctx)
		if err != nil || found == nil {
			cwd, _ := os.Getwd()
			report.add(codeWorkspaceNotFound, "workspace", "fail",
				fmt.Sprintf("no %s found from %s upward", resources.WorkspaceConfigurationName, cwd),
				"run inside a codefly workspace, or create one with `codefly init workspace <name>`")
			return nil
		}
		dir = *found
	} else if !resources.ExistsAtDir[resources.Workspace](dir) {
		report.add(codeWorkspaceNotFound, "workspace", "fail",
			fmt.Sprintf("no %s in %s", resources.WorkspaceConfigurationName, dir),
			"run inside a codefly workspace, or create one with `codefly init workspace <name>`")
		return nil
	}

	// Probe the raw YAML before the real load: loading a flat-layout workspace
	// that still carries a legacy module.codefly.yaml migrates it on disk
	// (rewrites workspace.codefly.yaml, deletes the module file). The doctor is
	// strictly read-only, so that state is reported instead of loaded.
	raw, err := os.ReadFile(filepath.Join(dir, resources.WorkspaceConfigurationName))
	if err != nil {
		report.add(codeWorkspaceInvalid, "workspace", "fail",
			fmt.Sprintf("cannot read %s: %v", resources.WorkspaceConfigurationName, err),
			"check file permissions on the workspace directory")
		return nil
	}
	var probe struct {
		Layout string `yaml:"layout"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		report.add(codeWorkspaceInvalid, "workspace", "fail",
			fmt.Sprintf("%s is not valid YAML: %v", resources.WorkspaceConfigurationName, err),
			"fix the workspace manifest syntax")
		return nil
	}
	if probe.Layout == resources.LayoutKindFlat {
		if _, statErr := os.Stat(filepath.Join(dir, resources.ModuleConfigurationName)); statErr == nil {
			report.add(codeWorkspaceInvalid, "workspace", "fail",
				fmt.Sprintf("flat-layout workspace still has a legacy %s pending migration; loading it would rewrite workspace files", resources.ModuleConfigurationName),
				"run `codefly update workspace` once in your primary checkout, commit the migration, then re-run")
			return nil
		}
	}

	ws, err := resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		report.add(codeWorkspaceInvalid, "workspace", "fail",
			fmt.Sprintf("cannot load workspace: %v", err),
			fmt.Sprintf("fix %s in %s", resources.WorkspaceConfigurationName, dir))
		return nil
	}
	report.Workspace = ws.Name
	report.WorkspaceDir = ws.Dir()
	report.add("", "workspace", "ok", fmt.Sprintf("%s (%s layout) at %s", ws.Name, ws.Layout, ws.Dir()), "")
	return ws
}

// checkReferencedModules reports every module declared by reference — a `path:`
// override pointing outside the vendored modules/<name>/ layout — and flags any
// whose target cannot be loaded. Without it an unresolved reference only
// surfaces later as an opaque "cannot load workspace services" failure; here the
// module and its resolved path are named directly. Vendored modules (no path
// override) and the implicit flat-layout module are skipped.
func checkReferencedModules(ctx context.Context, ws *resources.Workspace, report *workspaceReadinessReport) {
	for _, ref := range ws.Modules {
		if ref.PathOverride == nil {
			continue
		}
		resolved := ws.ModulePath(ctx, ref)
		if _, err := resources.LoadModuleFromDir(ctx, resolved); err != nil {
			report.add(codeModuleReferenceUnresolved, "referenced module "+ref.Name, "fail",
				fmt.Sprintf("referenced module %q does not resolve at %s: %v", ref.Name, resolved, err),
				fmt.Sprintf("fix the `path:` of module %q in %s, or drop the override so it resolves as its pinned module package", ref.Name, resources.WorkspaceConfigurationName))
			continue
		}
		report.add("", "referenced module "+ref.Name, "ok", fmt.Sprintf("%s → %s", ref.Name, resolved), "")
	}
}

// checkVendoredPins reports every module that states a pin — `source` plus
// `version` — and is satisfied by a local checkout rather than by the pinned
// artifact, whose checkout is not the version the pin names. Resolution drops
// Version the moment it takes a directory (`Kind: path`), so a checkout parked
// past the tag the pin names runs as if it were that tag, and nothing else in
// the workspace ever compares the two.
//
// Where the checkout comes from is asked of the resolver itself rather than
// re-derived here, so the committed `path:` of a vendored submodule and a
// machine-local `resolve.<name>.path` overlay — the same claim, one committed
// and one not — are both covered by the precedence the run path actually uses.
//
// Two resolutions are deliberately not compared:
//
//   - A path the receipt names is a materialization the CLI wrote, not a
//     checkout the user manages; codeModuleResolutionStale owns it, and
//     reporting both would double up on one condition.
//   - A worktree directive names its own git ref, and that ref — not the pin's
//     version — is what the user asked to run. Comparing `worktree: repo@main`
//     against a pinned version would warn on every run with no way to clear it.
func checkVendoredPins(ctx context.Context, ws *resources.Workspace, report *workspaceReadinessReport) {
	// An unreadable overlay or receipt record is reported by checkModuleTrust,
	// which owns those files; going quiet here loses no diagnostic.
	overlay, err := resources.LoadLocalOverlay(ctx, ws.Dir())
	if err != nil {
		return
	}
	overlayDir := ws.Dir()
	if dir := composition.NearestOverlayDir(ws.Dir()); dir != "" {
		overlayDir = dir
	}
	receipts, err := composition.LoadResolutionReceipts(overlayDir)
	if err != nil {
		return
	}
	// Resolved at most once, and only if some module turns out to be a
	// vendored pin: the workspace's own checkout root does not vary per module.
	workspaceRoot := sync.OnceValues(func() (string, bool) { return composition.CheckoutRoot(ctx, ws.Dir()) })
	for _, ref := range ws.Modules {
		if ref.Source == "" || ref.Version == "" {
			continue
		}
		directive := overlayDirective(overlay, ref.Name)
		if directive != nil && directive.Path != "" && directive.Path == receipts[ref.Name].ResolvedPath() {
			continue
		}
		resolution, err := ws.ResolveModule(ctx, ref)
		if err != nil || resolution.Dir == "" || resolution.Kind == resources.ResolutionWorktree {
			continue
		}
		checkVendoredPinVersion(ctx, workspaceRoot, ref, resolution.Dir, report)
	}
}

// checkVendoredPinVersion compares one resolved checkout against the version
// its pin names. It is a warning, not a failure: a checkout deliberately ahead
// of its tag is normal while developing the module, and only a problem when
// nobody notices.
//
// Only a checkout that is its own repository is compared. A path inside the
// workspace's own working tree is described by the workspace's tags, which say
// nothing about the module's version.
func checkVendoredPinVersion(ctx context.Context, workspaceRoot func() (string, bool), ref *resources.ModuleReference, resolved string, report *workspaceReadinessReport) {
	checkoutRoot, ok := composition.CheckoutRoot(ctx, resolved)
	if !ok {
		return
	}
	if root, known := workspaceRoot(); known && root == checkoutRoot {
		return
	}
	description, ok := composition.DescribeCheckout(ctx, resolved)
	if !ok || description.SatisfiesVersion(ref.Version) {
		return
	}
	report.add(codeModuleCheckoutVersionDrift, "vendored pin "+ref.Name, "warn",
		fmt.Sprintf("module %q pins %s at version %s, but its checkout %s is %s: the pin resolves to the checkout, so the declared version is not what runs", ref.Name, ref.Source, ref.Version, checkoutRoot, description.Raw),
		fmt.Sprintf("check out %s in %s, or update the `version:` of module %q in %s to what the checkout holds", ref.Version, checkoutRoot, ref.Name, resources.WorkspaceConfigurationName))
}

// checkServiceOverrides reports every per-service override the machine-local
// overlay declares. An override is invisible in committed config by design, so
// the active ones are listed even when they are healthy: a service quietly
// running from somewhere other than the module that composed it is the state
// this is here to make visible.
func checkServiceOverrides(ctx context.Context, ws *resources.Workspace, report *workspaceReadinessReport) {
	// An unreadable overlay is reported by checkModuleTrust, which owns the file.
	overlay, err := resources.LoadLocalOverlay(ctx, ws.Dir())
	if err != nil || overlay == nil {
		return
	}
	for _, ref := range ws.Modules {
		directive := overlayDirective(overlay, ref.Name)
		if directive == nil || len(directive.Services) == 0 {
			continue
		}
		resolution, err := ws.ResolveModule(ctx, ref)
		if err != nil {
			report.add(codeServiceOverrideUnresolved, "service overrides for "+ref.Name, "fail",
				fmt.Sprintf("the service overrides on module %q do not resolve: %v", ref.Name, err),
				fmt.Sprintf("fix resolve.%s.services in %s, or drop the entry with `codefly override service <module>/<service> --clear`", ref.Name, resources.LocalOverlayConfigurationName))
			continue
		}
		// Loaded straight from its directory, so it is the module as it declares
		// itself — the copy an override has to hold up against.
		declared, declaredErr := resources.LoadModuleFromDir(ctx, resolution.Dir)
		for _, service := range sortedServiceNames(directive.Services) {
			checkServiceOverride(ctx, ref, service, resolution.Services[service], declared, declaredErr, report)
		}
	}
}

func checkServiceOverride(ctx context.Context, ref *resources.ModuleReference, service string, resolved *resources.ServiceResolution, declared *resources.Module, declaredErr error, report *workspaceReadinessReport) {
	name := fmt.Sprintf("service override %s/%s", ref.Name, service)
	if resolved.Kind == resources.ResolutionPinned {
		report.add(codeServiceOverrideActive, name, "ok",
			fmt.Sprintf("service %q of module %q comes from module package %s at version %s; the next run materializes it", service, ref.Name, resolved.Source, resolved.Version),
			"")
		return
	}
	if !dirExists(resolved.Dir) {
		report.add(codeServiceOverrideUnresolved, name, "fail",
			fmt.Sprintf("service %q of module %q is overridden to %s, which does not exist", service, ref.Name, resolved.Dir),
			fmt.Sprintf("point resolve.%s.services.%s at a directory that holds the service, or drop it with `codefly override service %s/%s --clear`", ref.Name, service, ref.Name, service))
		return
	}
	if declaredErr != nil {
		// The module itself is not on disk yet — pinned, or unresolved — so there
		// is no composed copy to check the override against. It is still active,
		// and saying so beats reporting nothing about it at all.
		report.add(codeServiceOverrideActive, name, "ok",
			fmt.Sprintf("service %q of module %q runs from %s (%s override); module %q is not materialized, so the override could not be checked against the service it composes", service, ref.Name, resolved.Dir, resolved.Kind, ref.Name),
			"")
		return
	}
	declaredRef, err := declared.GetServiceReferences(service)
	if err != nil || declaredRef == nil {
		report.add(codeServiceOverrideUnresolved, name, "fail",
			fmt.Sprintf("module %q declares no service %q to override", ref.Name, service),
			fmt.Sprintf("remove resolve.%s.services.%s from %s", ref.Name, service, resources.LocalOverlayConfigurationName))
		return
	}
	// The same check the load performs, so doctor cannot pass an override the
	// next run refuses — or refuse one it accepts.
	if err := resources.CheckServiceOverrideContract(ctx, declared, declaredRef, resolved.Dir); err != nil {
		report.add(codeServiceOverrideDrift, name, "fail",
			err.Error(),
			fmt.Sprintf("make the service at %s match what module %q composes, or drop the override with `codefly override service %s/%s --clear`", resolved.Dir, ref.Name, ref.Name, service))
		return
	}
	report.add(codeServiceOverrideActive, name, "ok",
		fmt.Sprintf("service %q of module %q runs from %s (%s override)", service, ref.Name, resolved.Dir, resolved.Kind),
		"")
	checkServiceOverrideDrift(ctx, ref, service, resolved, report)
}

// checkServiceOverrideDrift compares a service taken from a checkout against the
// version its module pins, the same way a vendored module pin is compared: the
// override decides what runs, so a checkout that has moved away from the pinned
// version is running something the workspace does not declare.
func checkServiceOverrideDrift(ctx context.Context, ref *resources.ModuleReference, service string, resolved *resources.ServiceResolution, report *workspaceReadinessReport) {
	if resolved.Kind != resources.ResolutionWorktree || ref.Version == "" {
		return
	}
	checkoutRoot, ok := composition.CheckoutRoot(ctx, resolved.Dir)
	if !ok {
		return
	}
	description, ok := composition.DescribeCheckout(ctx, resolved.Dir)
	if !ok || description.SatisfiesVersion(ref.Version) {
		return
	}
	report.add(codeModuleCheckoutVersionDrift, fmt.Sprintf("service override %s/%s", ref.Name, service), "warn",
		fmt.Sprintf("module %q pins %s at version %s, but service %q is taken from checkout %s, which is %s: the override decides what runs, so the declared version is not what runs", ref.Name, ref.Source, ref.Version, service, checkoutRoot, description.Raw),
		fmt.Sprintf("check out %s in %s, or drop the override with `codefly override service %s/%s --clear`", ref.Version, checkoutRoot, ref.Name, service))
}

// sortedServiceNames orders the overridden service names so a report lists them
// the same way on every run.
func sortedServiceNames(services map[string]*resources.ServiceResolveDirective) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// checkModuleTrust reports every module pinned by `source@version` that a run
// would refuse to resolve: composition.ResolvePinnedModule fails closed when
// the workspace's module-trust doesn't cover the module's repository (whether
// because no module-trust is declared at all, or because it exists but names
// a different repository/package), and the module has not opted into the
// unverified git-clone fallback (`resolve.<name>.git: true`, or the
// composition.GitResolvedRecordName record `run` writes when it replaces that
// directive with the clone's path — both read from the nearest ancestor
// codefly.local.yaml exactly as `run` resolves it, not just the workspace's own
// directory). Without this check that failure only surfaces mid-run; here it is
// named against the workspace manifest directly, before a run is attempted. A
// read/parse error in workspace.codefly.yaml itself is reported once rather
// than once per pinned module.
//
// An opted-out module is not silently skipped: it is reported as consuming an
// unverified clone, because after `run` has replaced `git: true` with the
// resolved path the overlay no longer says so on its own.
//
// The workspace's own committed `module-resolution` entry is the third way a module can
// be opted out, and the only one that survives in a fresh checkout. It is
// reported twice on purpose: `module_resolution_git` states the declaration and
// how to leave it, `module_unverified` states the consequence, which is the same
// consequence however the clone was selected and must not read as milder
// because the workspace wrote it down.
func checkModuleTrust(ctx context.Context, ws *resources.Workspace, report *workspaceReadinessReport) {
	if _, _, err := composition.LoadModuleTrust(ws.Dir()); err != nil {
		report.add(codeModuleTrustMissing, "module-trust", "fail",
			fmt.Sprintf("cannot read module-trust from %s: %v", resources.WorkspaceConfigurationName, err),
			fmt.Sprintf("fix %s in %s", resources.WorkspaceConfigurationName, ws.Dir()))
		return
	}
	declared, err := composition.LoadModuleResolutions(ws.Dir())
	if err != nil {
		report.add(codeWorkspaceInvalid, "module resolution", "fail",
			fmt.Sprintf("cannot read module resolution from %s: %v", resources.WorkspaceConfigurationName, err),
			fmt.Sprintf("fix %s in %s", resources.WorkspaceConfigurationName, ws.Dir()))
		return
	}
	overlay, err := resources.LoadLocalOverlay(ctx, ws.Dir())
	if err != nil {
		report.add(codeWorkspaceInvalid, "module-trust", "fail",
			fmt.Sprintf("cannot read %s: %v", resources.LocalOverlayConfigurationName, err),
			fmt.Sprintf("fix %s near %s", resources.LocalOverlayConfigurationName, ws.Dir()))
		return
	}
	overlayDir := ws.Dir()
	if dir := composition.NearestOverlayDir(ws.Dir()); dir != "" {
		overlayDir = dir
	}
	receipts, err := composition.LoadResolutionReceipts(overlayDir)
	if err != nil {
		report.add(codeWorkspaceInvalid, "module-trust", "fail",
			fmt.Sprintf("cannot read %s: %v", composition.ResolutionRecordName, err),
			fmt.Sprintf("fix or delete %s in %s", composition.ResolutionRecordName, overlayDir))
		return
	}
	for _, ref := range ws.Modules {
		if ref.Source == "" || ref.PathOverride != nil {
			continue
		}
		directive := overlayDirective(overlay, ref.Name)
		receipt := receipts[ref.Name]
		mode := composition.ResolutionModeFor(directive, receipt, declared[ref.Name])
		checkModuleMaterialization(ref, directive, receipt, mode, report)
		if mode == composition.ResolutionModeDeclaredGit {
			report.add(codeModuleResolutionGit, "resolution of "+ref.Name, "ok",
				fmt.Sprintf("module %q is declared %s: git in %s: it resolves by cloning %s at its %s tag, unverified by declaration", ref.Name, composition.ModuleResolutionKey, resources.WorkspaceConfigurationName, ref.Source, moduleVersionLabel(ref)),
				fmt.Sprintf("once %s publishes a signed module package, add module-trust.repositories/signers for %q and drop it from %s — nothing else about the entry changes", ref.Source, ref.Name, composition.ModuleResolutionKey))
		}
		if mode.Unverified() {
			report.add(codeModuleUnverified, "module-trust for "+ref.Name, "warn",
				fmt.Sprintf("module %q resolves through the unverified git clone: nothing about it is signature- or digest-checked", ref.Name),
				unverifiedRemediation(ref.Name, mode))
			continue
		}
		if err := composition.CheckModuleTrustCoverage(ws.Dir(), ref); err != nil {
			report.add(codeModuleTrustMissing, "module-trust for "+ref.Name, "fail",
				fmt.Sprintf("module %q is pinned but not resolvable under module-trust: %v", ref.Name, err),
				fmt.Sprintf("add module-trust.repositories/signers for %q to %s, or set resolve.%s.git: true in %s to use the unverified git clone", ref.Name, resources.WorkspaceConfigurationName, ref.Name, resources.LocalOverlayConfigurationName))
		}
	}
}

// checkModulesMaterialized reports every composed pinned module that no
// materializing command has pulled onto this machine yet, and returns whether
// the service-scoped checks can run at all. The doctor never writes, so it
// cannot materialize the module itself the way `run`, `deploy gitops render`
// or `ci` would; what it can do is say exactly that, with the command that
// does, instead of relaying core's "pinned modules are pulled by the CLI, not
// loadable as a local checkout" out of the service load. A materialization the
// overlay still selects but which is gone from disk is the same condition with
// a different cause, and is named with the path that vanished.
func checkModulesMaterialized(ctx context.Context, ws *resources.Workspace, report *workspaceReadinessReport) bool {
	missing, err := composition.UnmaterializedModules(ctx, ws)
	if err != nil {
		// The files this reads are owned and reported by checkModuleTrust; a
		// second diagnostic for the same unreadable file adds nothing.
		return true
	}
	for _, module := range missing {
		message := fmt.Sprintf("module %q is declared but not materialized yet: nothing has pulled it into the module cache on this machine, so its services cannot be loaded", module.Name)
		if module.MissingPath != "" {
			message = fmt.Sprintf("module %q is declared but not materialized yet: %s selects %s for it, and that directory is gone or empty", module.Name, resources.LocalOverlayConfigurationName, module.MissingPath)
		}
		report.add(codeModuleNotMaterialized, "materialization of "+module.Name, "fail",
			message,
			fmt.Sprintf("run `codefly run service <service>` or `codefly deploy gitops render <module> --env <env>` once: either materializes every declared module and records it in %s, which doctor never writes; the service-scoped checks are skipped until then", resources.LocalOverlayConfigurationName))
	}
	return len(missing) == 0
}

// unverifiedRemediation names the file the opt-out actually lives in, so the
// advice is something the reader can act on: a declaration is dropped from the
// committed workspace manifest, a machine-local opt-out from the record `run`
// wrote when it consumed the directive.
func unverifiedRemediation(name string, mode composition.ResolutionMode) string {
	if mode == composition.ResolutionModeDeclaredGit {
		return fmt.Sprintf("add module-trust.repositories/signers for %q to %s and drop %q from its %s block to resolve it verified", name, resources.WorkspaceConfigurationName, name, composition.ModuleResolutionKey)
	}
	return fmt.Sprintf("add module-trust.repositories/signers for %q to %s and drop it from %s to resolve it verified", name, resources.WorkspaceConfigurationName, composition.ResolutionRecordName)
}

// moduleVersionLabel names the version a reference requests, spelling an absent
// one as what it means rather than as nothing.
func moduleVersionLabel(ref *resources.ModuleReference) string {
	if strings.TrimSpace(ref.Version) == "" {
		return "latest"
	}
	return ref.Version
}

// checkModuleMaterialization reports an overlay entry that still points at a
// materialization the CLI wrote for a *different* request than the workspace
// makes now — the state a version bump leaves behind between the edit and the
// next run. `run` refuses to reuse it (it re-resolves, and fails closed if that
// resolution fails), so this is a warning about what the next run will have to
// do rather than a failure in itself; without it the mismatch is invisible until
// the run either re-pulls or refuses. Only a path the receipt itself names is
// considered: any other path is a checkout the user manages, which no committed
// version says anything about.
func checkModuleMaterialization(ref *resources.ModuleReference, directive *resources.ModuleResolveDirective, receipt *composition.ResolutionReceipt, mode composition.ResolutionMode, report *workspaceReadinessReport) {
	if directive == nil || directive.Path == "" || directive.Path != receipt.ResolvedPath() {
		return
	}
	if receipt.Answers(ref, mode) {
		return
	}
	report.add(codeModuleResolutionStale, "materialization of "+ref.Name, "warn",
		fmt.Sprintf("module %q is materialized at %s for %s, but the workspace now requests %s: the next run re-resolves it and refuses that checkout if the request cannot be resolved", ref.Name, directive.Path, receiptRequestLabel(receipt), moduleRequestLabel(ref, mode)),
		fmt.Sprintf("run `codefly run solution` to re-resolve %q, or set resolve.%s.path in %s to a local checkout", ref.Name, ref.Name, resources.LocalOverlayConfigurationName))
}

// receiptRequestLabel describes the request a receipt answered. A receipt
// migrated from the pre-receipt record records no request at all, so it can only
// be described as one.
func receiptRequestLabel(receipt *composition.ResolutionReceipt) string {
	if receipt.Requested == "" && receipt.Version == "" {
		return "an unrecorded request"
	}
	return fmt.Sprintf("%s (%s, resolved to %s)", receipt.Requested, receipt.Mode, receipt.Version)
}

func moduleRequestLabel(ref *resources.ModuleReference, mode composition.ResolutionMode) string {
	return fmt.Sprintf("%s (%s)", moduleVersionLabel(ref), mode)
}

// overlayDirective returns module's overlay entry, or nil when there is no
// overlay at all.
func overlayDirective(overlay *resources.LocalOverlay, module string) *resources.ModuleResolveDirective {
	if overlay == nil {
		return nil
	}
	return overlay.Resolve[module]
}

func checkEnvironment(ws *resources.Workspace, name string, report *workspaceReadinessReport) *environments.Environment {
	env := ws.FindEnvironment(name)
	if env == nil {
		var declared []string
		for _, e := range ws.Environments {
			declared = append(declared, e.Name)
		}
		detail := "no environments are declared"
		if len(declared) > 0 {
			detail = "declared environments: " + strings.Join(declared, ", ")
		}
		report.add(codeEnvironmentNotFound, "environment", "fail",
			fmt.Sprintf("environment %q is not declared in %s (%s)", name, resources.WorkspaceConfigurationName, detail),
			fmt.Sprintf("declare %q under `environments:` in %s", name, resources.WorkspaceConfigurationName))
		return nil
	}
	for _, e := range ws.Environments {
		if e.Name == name {
			report.EnvironmentDeclared = true
		}
	}
	if report.EnvironmentDeclared {
		report.add("", "environment", "ok", fmt.Sprintf("%s (declared in %s)", env.Name, resources.WorkspaceConfigurationName), "")
	} else {
		report.add("", "environment", "ok", fmt.Sprintf("%s (implicit — not declared in %s)", env.Name, resources.WorkspaceConfigurationName), "")
	}
	deployment, err := environments.FromRuntime(env)
	if err != nil {
		report.add(codeEnvironmentNotFound, "environment", "fail", err.Error(), "correct the environment declarations")
		return nil
	}
	return deployment
}

// checkProviderBindings validates external provider bindings declared for the
// environment in provider-bindings.codefly.yaml. It is bounded and offline: it
// parses the document and validates each binding's identity, mode, secrets
// hygiene, output contract, and endpoint references. It never starts a provider
// agent or reaches the network. A missing document is fine — providers are
// opt-in; an unknown-schema document is reported so an old CLI does not silently
// ignore newer bindings.
func checkProviderBindings(ctx context.Context, ws *resources.Workspace, env *environments.Environment, report *workspaceReadinessReport) {
	doc, present, err := hostprovider.LoadDocument(ws.Dir())
	if err != nil {
		code := hostprovider.CodeBindingsUnreadable
		if errors.Is(err, hostprovider.ErrUnknownSchema) {
			code = hostprovider.CodeBindingsSchemaUnknown
		}
		report.add(code, "provider bindings", "fail", err.Error(), "fix or upgrade "+hostprovider.BindingsFileName)
		return
	}
	if !present {
		return
	}

	registry := configuration.NewRegistry()
	for _, binding := range doc.ForEnvironment(env.Name) {
		diagnostics := hostprovider.ValidateBinding(ctx, binding, registry)
		if len(diagnostics) == 0 {
			report.add("", "provider binding "+binding.Name, "ok",
				fmt.Sprintf("%s → %s [%s]", binding.Name, binding.Provider, binding.Mode), "")
			continue
		}
		for _, diagnostic := range diagnostics {
			report.add(diagnostic.Code, "provider binding "+binding.Name, "fail", diagnostic.Message,
				"correct the binding in "+hostprovider.BindingsFileName)
		}
	}
}

// checkSecretProviders validates the environment's declared secret backends
// and their executables. It returns the usable resolvers by scheme plus the
// schemes whose backend is declared but unusable (executable missing) — those
// are already reported, so reference resolution skips them silently.
func checkSecretProviders(env *environments.Environment, report *workspaceReadinessReport) (map[string]configurations.SecretResolver, map[string]bool) {
	resolvers, err := configurations.ResolversFromEnvironment(env.Runtime())
	if err != nil {
		report.add(codeProviderNotConfigured, "secret providers", "fail",
			err.Error(),
			fmt.Sprintf("fix the `secrets:` block of environment %q in %s (supported kind: %s)",
				env.Name, resources.WorkspaceConfigurationName, configurations.ProviderOnePassword))
		return nil, nil
	}
	byScheme := make(map[string]configurations.SecretResolver)
	for _, r := range resolvers {
		if _, ok := byScheme[r.Scheme()]; !ok {
			byScheme[r.Scheme()] = r
		}
	}
	unavailable := make(map[string]bool)
	if len(env.Secrets) == 0 {
		report.add("", "secret providers", "ok", "none declared (secret files are used as-is)", "")
		return byScheme, unavailable
	}
	var described []string
	for _, provider := range env.Secrets {
		// ResolversFromEnvironment already rejected unknown kinds.
		if provider.Kind == configurations.ProviderOnePassword {
			binPath, lookErr := exec.LookPath("op")
			if lookErr != nil {
				report.add(codeProviderExecutableMissing, "secret providers", "fail",
					fmt.Sprintf("environment %q uses the %s backend but the `op` CLI is not on PATH", env.Name, provider.Kind),
					"install the 1Password CLI (https://developer.1password.com/docs/cli/) and sign in with `op signin`")
				delete(byScheme, configurations.OnePasswordScheme)
				unavailable[configurations.OnePasswordScheme] = true
				continue
			}
			described = append(described, fmt.Sprintf("%s (op at %s)", provider.Kind, binPath))
		}
	}
	if len(described) > 0 {
		report.add("", "secret providers", "ok", strings.Join(described, ", "), "")
	}
	return byScheme, unavailable
}

// checkScope loads the services under validation — all of them, the ones of the
// module selected with --module, or the one selected with --service — and
// collects the workspace configurations they declare, mapped to the services
// that require them.
//
// --module and --service narrow the same way for the same reason: a verb that
// acts on one unit must not be judged by a sibling unit's missing
// configuration. --service wins when both are given, being the narrower of the
// two.
func checkScope(ctx context.Context, ws *resources.Workspace, moduleName, serviceName string, report *workspaceReadinessReport) ([]*resources.Service, map[string][]string) {
	services, err := ws.LoadServices(ctx)
	if err != nil {
		report.add(codeWorkspaceInvalid, "services", "fail",
			fmt.Sprintf("cannot load workspace services: %v", err),
			"fix the module/service manifests referenced by the workspace")
		return nil, nil
	}
	scope := services
	if moduleName != "" && serviceName == "" {
		mod, modErr := ws.LoadModuleFromName(ctx, moduleName)
		if modErr != nil {
			available := ws.ModulesNames()
			sort.Strings(available)
			report.add(codeModuleNotFound, "module", "fail",
				fmt.Sprintf("cannot find module %q: %v", moduleName, modErr),
				"workspace modules: "+strings.Join(available, ", "))
			return nil, nil
		}
		moduleServices, svcErr := mod.LoadServices(ctx)
		if svcErr != nil {
			report.add(codeWorkspaceInvalid, "services", "fail",
				fmt.Sprintf("cannot load services of module %q: %v", moduleName, svcErr),
				"fix the service manifests referenced by the module")
			return nil, nil
		}
		for _, svc := range moduleServices {
			svc.WithModule(mod.Name)
		}
		scope = moduleServices
		report.Module = mod.Name
	}
	if serviceName != "" {
		svc, mod, findErr := ws.FindUniqueModuleServiceByName(ctx, serviceName)
		if findErr != nil {
			var available []string
			for _, s := range services {
				available = append(available, s.Name)
			}
			sort.Strings(available)
			report.add(codeServiceNotFound, "service", "fail",
				fmt.Sprintf("cannot find service %q: %v", serviceName, findErr),
				"available services: "+strings.Join(available, ", "))
			return nil, nil
		}
		if mod != nil {
			svc.WithModule(mod.Name)
		}
		scope = []*resources.Service{svc}
		report.Service = serviceUnique(svc)
	}
	requiredBy := make(map[string][]string)
	for _, svc := range scope {
		for _, dep := range svc.WorkspaceConfigurationDependencies {
			requiredBy[dep] = append(requiredBy[dep], serviceUnique(svc))
		}
	}
	report.add("", "services", "ok", fmt.Sprintf("%d service(s) in scope", len(scope)), "")
	return scope, requiredBy
}

func serviceUnique(svc *resources.Service) string {
	if identity, err := svc.Identity(); err == nil {
		return identity.Unique()
	}
	return svc.Name
}

type scopedConfiguration struct {
	origin string // "workspace", "module <name>", or the service unique
	info   *basev0.ConfigurationInformation
}

// checkConfigurationSources discovers the configurations the environment
// provides, exactly as a run provisions them: the workspace's own
// configurations/<profile>/* composed with the ones each composed module ships
// in its tree — through core's configurations.ReadWorkspaceConfigurations, the
// one definition of that rule, so the doctor cannot disagree with `run` — and
// then the per-service directories. It never creates a directory: a missing
// workspace directory is a failure only for the groups no composed module
// provides. It returns the configurations whose secret values are in scope for
// reference resolution.
func checkConfigurationSources(ctx context.Context, ws *resources.Workspace, env *environments.Environment, serviceScoped bool, scope []*resources.Service, requiredBy map[string][]string, report *workspaceReadinessReport) []scopedConfiguration {
	required := make([]string, 0, len(requiredBy))
	for name := range requiredBy {
		required = append(required, name)
	}
	sort.Strings(required)

	var toResolve []scopedConfiguration

	// The profile, not the environment name, selects the directory — the same
	// choice core makes when the run loads.
	runtimeEnv := env.Runtime()
	profile, err := runtimeEnv.ConfigurationProfileName()
	if err != nil {
		report.add(codeEnvironmentNotFound, "environment", "fail",
			fmt.Sprintf("environment %q selects an invalid configuration profile: %v", env.Name, err),
			fmt.Sprintf("fix `configuration-profile` of environment %q in %s", env.Name, resources.WorkspaceConfigurationName))
		return nil
	}
	wsCfgDir := filepath.Join(ws.Dir(), "configurations", profile)
	relCfgDir := filepath.Join("configurations", profile)

	if provided := loadWorkspaceConfigurations(ctx, ws, runtimeEnv, relCfgDir, report); provided != nil {
		byName := make(map[string]*basev0.ConfigurationInformation, len(provided.Infos))
		var own, composed []*basev0.ConfigurationInformation
		for _, info := range provided.Infos {
			byName[info.Name] = info
			if _, ok := provided.ComposedBy[info.Name]; ok {
				composed = append(composed, info)
			} else {
				own = append(own, info)
			}
		}
		// Only what nobody provides — a name no module ships, or one two modules
		// disagree on — is what the workspace directory would have to hold; a
		// module-shipped group is satisfied without it, exactly as in a run.
		var unprovided []string
		for _, name := range required {
			if _, ok := byName[name]; !ok {
				unprovided = append(unprovided, name)
			}
		}
		switch {
		case !dirExists(wsCfgDir) && len(unprovided) > 0:
			report.add(codeConfigurationDirMissing, "workspace configurations", "fail",
				fmt.Sprintf("%s does not exist and %d required workspace configuration(s) are provided by no composed module: %s", relCfgDir, len(unprovided), strings.Join(unprovided, ", ")),
				fmt.Sprintf("create %s/ and add the required configuration files — fresh worktrees do not carry ignored *.secret.env files; copy the reference files from your primary checkout (secret values stay in the provider)", relCfgDir))
		case !dirExists(wsCfgDir) && len(required) > 0:
			report.add("", "workspace configurations", "ok",
				fmt.Sprintf("none present under %s (all %d required configuration(s) are provided by composed modules)", relCfgDir, len(required)), "")
		case !dirExists(wsCfgDir):
			report.add("", "workspace configurations", "ok", fmt.Sprintf("none present under %s (none required)", relCfgDir), "")
		case len(own) == 0:
			report.add("", "workspace configurations", "ok", fmt.Sprintf("none under %s", relCfgDir), "")
		default:
			report.add("", "workspace configurations", "ok",
				fmt.Sprintf("%d configuration(s) under %s: %s", len(own), relCfgDir, infoNames(own)), "")
		}
		if len(composed) > 0 {
			report.add("", "composed module configurations", "ok",
				fmt.Sprintf("%d configuration(s) shipped by composed modules: %s", len(composed), providedNames(composed, provided.ComposedBy)), "")
		}
		for _, name := range required {
			requiredByList := strings.Join(requiredBy[name], ", ")
			if conflict, ok := provided.Ambiguous[name]; ok {
				report.add(codeConfigurationDuplicate, "workspace configurations", "fail",
					fmt.Sprintf("required workspace configuration %q is ambiguous (required by %s): %v", name, requiredByList, conflict),
					fmt.Sprintf("add %s/%s.env to this workspace: its own definition overrides every composed module's", relCfgDir, name))
				continue
			}
			info, ok := byName[name]
			if !ok {
				report.add(codeConfigurationMissing, "workspace configurations", "fail",
					fmt.Sprintf("required workspace configuration %q is neither under %s nor shipped by a composed module (required by %s)", name, relCfgDir, requiredByList),
					fmt.Sprintf("add %s/%s.env (or %s.secret.env holding provider references)", relCfgDir, name, name))
				continue
			}
			module, fromModule := provided.ComposedBy[name]
			if len(info.ConfigurationValues) == 0 && info.Data == nil {
				where, remedy := "exists under "+relCfgDir, fmt.Sprintf("populate the %s files for %q with the required keys", relCfgDir, name)
				if fromModule {
					where = fmt.Sprintf("is shipped by composed module %q", module)
					remedy = fmt.Sprintf("add %s/%s.env with the required keys: the workspace's own definition overrides the module's", relCfgDir, name)
				}
				report.add(codeConfigurationMissing, "workspace configurations", "fail",
					fmt.Sprintf("workspace configuration %q %s but defines no values (required by %s)", name, where, requiredByList), remedy)
				continue
			}
			if fromModule {
				report.add("", "workspace configuration "+name, "ok",
					fmt.Sprintf("provided by composed module %q (required by %s)", module, requiredByList), "")
			}
		}
		if serviceScoped {
			// Resolve only what the selected service declares; unrelated
			// workspace configurations must not be touched.
			for _, name := range required {
				if info, ok := byName[name]; ok {
					toResolve = append(toResolve, scopedConfiguration{origin: workspaceConfigurationOrigin(name, provided.ComposedBy), info: info})
				}
			}
		} else {
			for _, info := range provided.Infos {
				toResolve = append(toResolve, scopedConfiguration{origin: workspaceConfigurationOrigin(info.Name, provided.ComposedBy), info: info})
			}
		}
	}

	serviceConfigurations := 0
	for _, svc := range scope {
		svcCfgDir := filepath.Join(svc.Dir(), "configurations", profile)
		label := fmt.Sprintf("service %s", serviceUnique(svc))
		relSvcDir := filepath.Join(svc.Name, "configurations", profile)
		if !dirExists(svcCfgDir) {
			if others := otherEnvironmentDirs(filepath.Join(svc.Dir(), "configurations"), profile); len(others) > 0 {
				report.add(codeConfigurationDirMissing, "service configurations", "warn",
					fmt.Sprintf("%s has configurations for %s but none for environment %q", label, strings.Join(others, ", "), env.Name),
					fmt.Sprintf("create %s/ if this service needs configuration in %q", relSvcDir, env.Name))
			}
			continue
		}
		infos, ok := loadConfigurationDir(ctx, svcCfgDir, label, relSvcDir, report)
		if !ok {
			continue
		}
		for _, info := range infos {
			toResolve = append(toResolve, scopedConfiguration{origin: serviceUnique(svc), info: info})
		}
		serviceConfigurations += len(infos)
	}
	if serviceConfigurations > 0 {
		report.add("", "service configurations", "ok", fmt.Sprintf("%d configuration(s) across %d service(s)", serviceConfigurations, len(scope)), "")
	}
	return toResolve
}

// loadWorkspaceConfigurations reads what the environment's profile provides to
// the workspace through the same core read a run provisions from — which never
// creates the workspace directory; its absence is judged by the caller. A tree
// that cannot be read, in the workspace's own directory or in a composed
// module's, is reported and returns nil. Duplicate keys within one
// configuration are flagged as for a service directory.
func loadWorkspaceConfigurations(ctx context.Context, ws *resources.Workspace, env *resources.Environment, relCfgDir string, report *workspaceReadinessReport) *configurations.WorkspaceConfigurations {
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, ws, env)
	if err != nil {
		code := codeConfigurationInvalid
		if errors.Is(err, configurations.ErrConfigurationConflict) {
			code = codeConfigurationDuplicate
		}
		report.add(code, "workspace configurations", "fail",
			fmt.Sprintf("cannot load workspace configurations from %s and the composed modules: %v", relCfgDir, err),
			fmt.Sprintf("fix the configuration files under %s, or the ones the named composed module ships", relCfgDir))
		return nil
	}
	sort.SliceStable(provided.Infos, func(i, j int) bool { return provided.Infos[i].Name < provided.Infos[j].Name })
	for _, info := range provided.Infos {
		where := relCfgDir
		if module, ok := provided.ComposedBy[info.Name]; ok {
			where = fmt.Sprintf("composed module %q", module)
		}
		reportDuplicateKeys(info, "workspace", where, report)
	}
	return provided
}

// workspaceConfigurationOrigin labels where a workspace configuration in scope
// for secret resolution came from.
func workspaceConfigurationOrigin(name string, composedBy map[string]string) string {
	if module, ok := composedBy[name]; ok {
		return "module " + module
	}
	return "workspace"
}

// providedNames lists composed configurations with the module shipping each.
func providedNames(infos []*basev0.ConfigurationInformation, composedBy map[string]string) string {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, fmt.Sprintf("%s (%s)", info.Name, composedBy[info.Name]))
	}
	return strings.Join(names, ", ")
}

// loadConfigurationDir reads one configurations/<env> directory read-only.
// A missing directory returns (nil, true) — absence is judged by the caller.
// Parse failures are reported and return ok=false.
func loadConfigurationDir(ctx context.Context, dir, label, relDir string, report *workspaceReadinessReport) ([]*basev0.ConfigurationInformation, bool) {
	if !dirExists(dir) {
		return nil, true
	}
	infos, err := configurations.LoadConfigurationInformationsFromFiles(ctx, dir)
	if err != nil {
		code := codeConfigurationInvalid
		if errors.Is(err, configurations.ErrConfigurationConflict) {
			code = codeConfigurationDuplicate
		}
		report.add(code, label+" configurations", "fail",
			fmt.Sprintf("cannot load configurations for %s from %s: %v", label, relDir, err),
			fmt.Sprintf("fix the configuration files under %s", relDir))
		return nil, false
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	for _, info := range infos {
		reportDuplicateKeys(info, label, relDir, report)
	}
	return infos, true
}

// reportDuplicateKeys flags a key one configuration defines more than once
// across its files.
func reportDuplicateKeys(info *basev0.ConfigurationInformation, label, relDir string, report *workspaceReadinessReport) {
	seen := make(map[string]bool)
	for _, value := range info.ConfigurationValues {
		if seen[value.Key] {
			report.add(codeConfigurationDuplicate, label+" configurations", "fail",
				fmt.Sprintf("configuration %q under %s defines key %s more than once", info.Name, relDir, value.Key),
				fmt.Sprintf("keep a single definition of %s across the %q files", value.Key, info.Name))
		}
		seen[value.Key] = true
	}
}

// checkSecretReferences resolves every secret provider reference in scope
// through the environment's configured backend, in memory, and discards the
// values immediately. This is the behavior being diagnosed: a locked or
// unavailable provider must fail here, not halfway through `codefly run`.
func checkSecretReferences(ctx context.Context, env *environments.Environment, resolvers map[string]configurations.SecretResolver, unavailable map[string]bool, toResolve []scopedConfiguration, report *workspaceReadinessReport) {
	failuresBefore := len(report.Checks)
	resolved, plaintext := 0, 0
	attempted := make(map[string]bool)
	warnedPlaintext := make(map[string]bool)
	timedOut := false

	checkValue := func(name, key, value string) {
		if timedOut {
			return
		}
		where := fmt.Sprintf("key %s of configuration %q", key, name)
		ref, isRef := configurations.ParseSecretReference(value)
		if !isRef {
			if scheme, looksRef := unknownReferenceScheme(value); looksRef {
				report.add(codeReferenceSchemeUnknown, "secret references", "warn",
					fmt.Sprintf("%s looks like a secret reference with unsupported scheme %q — codefly would use it as plaintext", where, scheme),
					fmt.Sprintf("supported reference scheme: %s:// ; if the value is intentional plaintext, ignore this warning", configurations.OnePasswordScheme))
				return
			}
			plaintext++
			if len(resolvers) > 0 && !env.Local() && !warnedPlaintext[name] {
				warnedPlaintext[name] = true
				report.add(codePlaintextNotAllowed, "secret references", "warn",
					fmt.Sprintf("configuration %q holds plaintext secret values although environment %q declares a secret backend", name, env.Name),
					fmt.Sprintf("replace the plaintext values with %s:// references", configurations.OnePasswordScheme))
			}
			return
		}
		if unavailable[ref.Scheme] {
			return
		}
		resolver, ok := resolvers[ref.Scheme]
		if !ok {
			report.add(codeProviderNotConfigured, "secret references", "fail",
				fmt.Sprintf("%s is a %s:// reference but environment %q does not configure that backend", where, ref.Scheme, env.Name),
				fmt.Sprintf("declare the backend under `environments:` in %s, e.g. `secrets: [{kind: %s}]`", resources.WorkspaceConfigurationName, configurations.ProviderOnePassword))
			return
		}
		if attempted[ref.Raw] {
			return
		}
		attempted[ref.Raw] = true
		value, err := resolver.Resolve(ctx, ref)
		_ = value // resolved for validation only; discarded immediately
		if err == nil {
			resolved++
			return
		}
		if ctx.Err() != nil {
			timedOut = true
			report.add(codeTimeout, "secret references", "fail",
				fmt.Sprintf("secret resolution did not finish in time (stopped at %s)", where),
				"the provider may be locked or waiting for interactive authentication; unlock it (e.g. `op signin`) or raise --timeout, then re-run")
			return
		}
		// Provider output is never echoed: it can contain the reference URI or
		// item identifiers. Only the location of the failing reference is safe.
		if authenticationError(err) {
			report.add(codeProviderAuthRequired, "secret references", "fail",
				fmt.Sprintf("the %s backend requires authentication (failed on %s)", ref.Scheme, where),
				"sign in to the provider (e.g. `op signin`) and re-run"+accountHint(env))
			return
		}
		report.add(codeProviderResolutionFailed, "secret references", "fail",
			fmt.Sprintf("cannot resolve the %s:// reference at %s", ref.Scheme, where),
			"run the provider CLI on that reference yourself (e.g. `op read <reference>`) to see its error; never paste the resolved value")
	}

	for _, scoped := range toResolve {
		for _, value := range scoped.info.ConfigurationValues {
			if value.Secret {
				checkValue(scoped.info.Name, value.Key, value.Value)
			}
		}
		if scoped.info.Data != nil && scoped.info.Data.Secret {
			leaves, err := secretDataStrings(scoped.info.Data)
			if err != nil {
				report.add(codeConfigurationInvalid, "secret references", "fail",
					fmt.Sprintf("secret configuration %q is not valid %s", scoped.info.Name, scoped.info.Data.Kind),
					fmt.Sprintf("fix the %q secret file", scoped.info.Name))
				continue
			}
			for _, leaf := range leaves {
				checkValue(scoped.info.Name, "(structured value)", leaf)
			}
		}
	}

	if len(report.Checks) == failuresBefore {
		report.add("", "secret references", "ok",
			fmt.Sprintf("%d reference(s) resolved in memory (values discarded), %d plaintext secret value(s)", resolved, plaintext), "")
	}
}

func accountHint(env *environments.Environment) string {
	for _, provider := range env.Secrets {
		if provider.Account != "" {
			return fmt.Sprintf(" (account %q)", provider.Account)
		}
	}
	return ""
}

func authenticationError(err error) bool {
	return errors.Is(err, configurations.ErrSecretProviderAuthenticationRequired)
}

// unknownReferenceScheme reports a value shaped like a provider reference
// (scheme://…) whose scheme codefly does not support — those are silently
// treated as plaintext at runtime, which is almost never what the author
// meant. Common data-connection URI schemes are exempt.
var referenceSchemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*$`)

var plaintextURISchemes = map[string]bool{
	"amqp": true, "amqps": true, "bolt": true, "clickhouse": true, "cockroachdb": true,
	"file": true, "ftp": true, "grpc": true, "grpcs": true, "http": true, "https": true,
	"imap": true, "jdbc": true, "kafka": true, "ldap": true, "ldaps": true, "memcached": true,
	"mongodb": true, "mongodb+srv": true, "mssql": true, "mysql": true, "nats": true,
	"neo4j": true, "oracle": true, "postgres": true, "postgresql": true, "redis": true,
	"rediss": true, "s3": true, "sftp": true, "smtp": true, "smtps": true, "sqlserver": true,
	"ssh": true, "tcp": true, "udp": true, "unix": true, "ws": true, "wss": true,
}

func unknownReferenceScheme(value string) (string, bool) {
	scheme, _, found := strings.Cut(value, "://")
	if !found || scheme == "" || len(scheme) > 16 || !referenceSchemePattern.MatchString(scheme) {
		return "", false
	}
	if plaintextURISchemes[strings.ToLower(scheme)] {
		return "", false
	}
	return scheme, true
}

// secretDataStrings extracts the string leaves of a structured secret blob
// (*.secret.yaml / *.secret.json) in deterministic order, mirroring the
// values core would attempt to resolve at Load() time.
func secretDataStrings(data *basev0.ConfigurationData) ([]string, error) {
	var node any
	switch data.Kind {
	case "yaml", "yml":
		if err := yaml.Unmarshal(data.Content, &node); err != nil {
			return nil, err
		}
	case "json":
		if err := json.Unmarshal(data.Content, &node); err != nil {
			return nil, err
		}
	default:
		return nil, nil
	}
	var out []string
	collectStrings(node, &out)
	return out, nil
}

func collectStrings(node any, out *[]string) {
	switch v := node.(type) {
	case string:
		*out = append(*out, v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectStrings(v[key], out)
		}
	case []any:
		for _, item := range v {
			collectStrings(item, out)
		}
	}
}

func dirExists(dir string) bool {
	stat, err := os.Stat(dir)
	return err == nil && stat.IsDir()
}

// otherEnvironmentDirs lists the environment subdirectories present under a
// configurations/ directory, excluding the requested one.
func otherEnvironmentDirs(configurationsDir, except string) []string {
	entries, err := os.ReadDir(configurationsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != except {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out
}

func infoNames(infos []*basev0.ConfigurationInformation) string {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	return strings.Join(names, ", ")
}

// machineReadableDoctorError signals main that the JSON payload on stdout is
// the complete diagnostic output; only the non-zero exit remains.
type machineReadableDoctorError struct{ error }

func (machineReadableDoctorError) MachineReadable() bool { return true }

var (
	doctorWorkspaceEnv     string
	doctorWorkspaceModule  string
	doctorWorkspaceService string
	doctorWorkspaceJSON    bool
	doctorWorkspaceTimeout time.Duration
)

var DoctorWorkspaceCmd = &cobra.Command{
	Use: "workspace",
	// A not-ready workspace is an expected outcome, not a usage mistake.
	SilenceUsage: true,
	Short:        "Validate workspace paths, configuration, and agent readiness without changes",
	Long: `Validate, from the current directory, that the workspace is ready for
` + "`codefly run` / `codefly test`" + ` — without starting anything.

The check discovers the workspace, resolves the selected environment
(default: local), validates declared secret backends and their executables,
verifies that required workspace and service configurations exist for the
environment, and resolves secret provider references (op://…) in memory,
discarding the values immediately. It is strictly read-only: it never creates
directories or files, never starts agents, containers, or services, and never
prints secret values or raw references.

Designed for fresh git worktrees, where ignored *.secret.env files are absent
and the failure would otherwise only surface mid-orchestration.

Exit codes:
  0  the workspace is ready (warnings allowed)
  1  at least one check failed, or the command itself failed

With --json, a versioned report is printed to stdout:
  {schema_version, workspace, workspace_dir, environment, environment_declared,
   module?, service?, status: ready|not_ready, checks: [{code, name, status,
   message, remediation?}]}

--module and --service narrow the scope to one unit's declared configuration
dependencies, so a sibling unit's missing configuration does not fail the
check. --service is the narrower of the two and wins when both are given.

Stable diagnostic codes: workspace_not_found, workspace_invalid,
environment_not_found, module_not_found, service_not_found,
module_reference_unresolved,
module_trust_missing, module_checkout_version_drift,
service_override_active, service_override_unresolved,
service_override_contract_drift,
configuration_directory_missing, configuration_missing,
configuration_invalid, configuration_duplicate, provider_not_configured,
provider_executable_missing, provider_authentication_required,
provider_resolution_failed, plaintext_not_allowed, reference_scheme_unknown,
timeout. External provider
binding checks add external_provider.* codes (bindings_unreadable,
bindings_schema_unknown, and the per-binding validation codes).`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		report := workspaceReadiness(cmd.Context(), workspaceReadinessOptions{
			env:     doctorWorkspaceEnv,
			module:  doctorWorkspaceModule,
			service: doctorWorkspaceService,
			timeout: doctorWorkspaceTimeout,
		})

		if doctorWorkspaceJSON {
			payload, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(payload))
			if report.Status != readinessStatusReady {
				return machineReadableDoctorError{errors.New("workspace is not ready")}
			}
			return nil
		}

		fmt.Println(tui.RenderHeader(1, "codefly doctor workspace"))
		for _, diagnostic := range report.Checks {
			printWorkspaceDiagnostic(diagnostic)
		}
		fmt.Println()
		if report.Status != readinessStatusReady {
			fmt.Println(tui.RenderError("Workspace is NOT ready — fix the items marked ✗ above."))
			return fmt.Errorf("workspace is not ready")
		}
		fmt.Println(tui.RenderInfo(fmt.Sprintf("Workspace %q is ready for environment %q.", report.Workspace, report.Environment)))
		return nil
	},
}

func printWorkspaceDiagnostic(diagnostic workspaceDiagnostic) {
	result := checkResult{name: diagnostic.Name, detail: diagnostic.Message, fix: diagnostic.Remediation}
	switch diagnostic.Status {
	case "warn":
		result.status = statusWarn
		result.detail = fmt.Sprintf("%s [%s]", diagnostic.Message, diagnostic.Code)
	case checkStatusFail:
		result.status = statusFail
		result.detail = fmt.Sprintf("%s [%s]", diagnostic.Message, diagnostic.Code)
	default:
		result.status = statusOK
	}
	result.print()
}

func init() {
	DoctorWorkspaceCmd.Flags().StringVar(&doctorWorkspaceEnv, "env", "local", "Environment to validate against")
	DoctorWorkspaceCmd.Flags().StringVar(&doctorWorkspaceModule, "module", "", "Restrict validation to one module's services and their declared configuration dependencies")
	DoctorWorkspaceCmd.Flags().StringVar(&doctorWorkspaceService, "service", "", "Restrict validation to one service's declared configuration dependencies")
	DoctorWorkspaceCmd.Flags().BoolVar(&doctorWorkspaceJSON, "json", false, "Print a machine-readable report to stdout")
	DoctorWorkspaceCmd.Flags().DurationVar(&doctorWorkspaceTimeout, "timeout", 30*time.Second, "Overall bound; secret resolution is cancelled when it expires")
	DoctorCmd.AddCommand(DoctorWorkspaceCmd)
}
