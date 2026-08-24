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
	"time"

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
	codeWorkspaceNotFound         = "workspace_not_found"
	codeWorkspaceInvalid          = "workspace_invalid"
	codeEnvironmentNotFound       = "environment_not_found"
	codeServiceNotFound           = "service_not_found"
	codeConfigurationDirMissing   = "configuration_directory_missing"
	codeConfigurationMissing      = "configuration_missing"
	codeConfigurationInvalid      = "configuration_invalid"
	codeConfigurationDuplicate    = "configuration_duplicate"
	codeProviderNotConfigured     = "provider_not_configured"
	codeProviderExecutableMissing = "provider_executable_missing"
	codeProviderAuthRequired      = "provider_authentication_required"
	codeProviderResolutionFailed  = "provider_resolution_failed"
	codePlaintextNotAllowed       = "plaintext_not_allowed"
	codeReferenceSchemeUnknown    = "reference_scheme_unknown"
	codeModuleReferenceUnresolved = "module_reference_unresolved"
	codeTimeout                   = "timeout"
)

const doctorWorkspaceSchemaVersion = 1

const (
	readinessStatusReady    = "ready"
	readinessStatusNotReady = "not_ready"
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
	Service             string                `json:"service,omitempty"`
	Status              string                `json:"status"`
	Checks              []workspaceDiagnostic `json:"checks"`
}

func (report *workspaceReadinessReport) add(code, name, status, message, remediation string) {
	report.Checks = append(report.Checks, workspaceDiagnostic{
		Code: code, Name: name, Status: status, Message: message, Remediation: remediation,
	})
	if status == "fail" {
		report.Status = readinessStatusNotReady
	}
}

type workspaceReadinessOptions struct {
	// dir pins the workspace directory (tests). Empty means the usual upward
	// discovery from the current directory.
	dir     string
	env     string
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

	env := checkEnvironment(ws, opts.env, report)
	if env == nil {
		return report
	}

	checkProviderBindings(ctx, ws, env, report)

	resolvers, unavailable := checkSecretProviders(env, report)

	scope, requiredBy := checkScope(ctx, ws, opts.service, report)
	if scope == nil {
		return report
	}

	toResolve := checkConfigurationSources(ctx, ws, env, opts.service != "", scope, requiredBy, report)

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
				fmt.Sprintf("fix the `path:` of module %q in %s, or vendor it with `codefly sync module`", ref.Name, resources.WorkspaceConfigurationName))
			continue
		}
		report.add("", "referenced module "+ref.Name, "ok", fmt.Sprintf("%s → %s", ref.Name, resolved), "")
	}
}

func checkEnvironment(ws *resources.Workspace, name string, report *workspaceReadinessReport) *resources.Environment {
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
	return env
}

// checkProviderBindings validates external provider bindings declared for the
// environment in provider-bindings.codefly.yaml. It is bounded and offline: it
// parses the document and validates each binding's identity, mode, secrets
// hygiene, output contract, and endpoint references. It never starts a provider
// agent or reaches the network. A missing document is fine — providers are
// opt-in; an unknown-schema document is reported so an old CLI does not silently
// ignore newer bindings.
func checkProviderBindings(ctx context.Context, ws *resources.Workspace, env *resources.Environment, report *workspaceReadinessReport) {
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
func checkSecretProviders(env *resources.Environment, report *workspaceReadinessReport) (map[string]configurations.SecretResolver, map[string]bool) {
	resolvers, err := configurations.ResolversFromEnvironment(env)
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

// checkScope loads the services under validation — all of them, or the one
// selected with --service — and collects the workspace configurations they
// declare, mapped to the services that require them.
func checkScope(ctx context.Context, ws *resources.Workspace, serviceName string, report *workspaceReadinessReport) ([]*resources.Service, map[string][]string) {
	services, err := ws.LoadServices(ctx)
	if err != nil {
		report.add(codeWorkspaceInvalid, "services", "fail",
			fmt.Sprintf("cannot load workspace services: %v", err),
			"fix the module/service manifests referenced by the workspace")
		return nil, nil
	}
	scope := services
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
	origin string // "workspace" or the service unique
	info   *basev0.ConfigurationInformation
}

// checkConfigurationSources discovers configuration files for the environment
// without ever creating directories (core's reader would mkdir a missing
// configurations/<env>; the doctor reports it instead). It returns the
// configurations whose secret values are in scope for reference resolution.
func checkConfigurationSources(ctx context.Context, ws *resources.Workspace, env *resources.Environment, serviceScoped bool, scope []*resources.Service, requiredBy map[string][]string, report *workspaceReadinessReport) []scopedConfiguration {
	required := make([]string, 0, len(requiredBy))
	for name := range requiredBy {
		required = append(required, name)
	}
	sort.Strings(required)

	var toResolve []scopedConfiguration

	wsCfgDir := filepath.Join(ws.Dir(), "configurations", env.Name)
	relCfgDir := filepath.Join("configurations", env.Name)
	wsInfos, loaded := loadConfigurationDir(ctx, wsCfgDir, "workspace", relCfgDir, report)
	switch {
	case loaded && wsInfos == nil && !dirExists(wsCfgDir):
		if len(required) > 0 {
			report.add(codeConfigurationDirMissing, "workspace configurations", "fail",
				fmt.Sprintf("%s does not exist but %d workspace configuration(s) are required: %s", relCfgDir, len(required), strings.Join(required, ", ")),
				fmt.Sprintf("create %s/ and add the required configuration files — fresh worktrees do not carry ignored *.secret.env files; copy the reference files from your primary checkout (secret values stay in the provider)", relCfgDir))
		} else {
			report.add("", "workspace configurations", "ok", fmt.Sprintf("none present under %s (none required)", relCfgDir), "")
		}
	case loaded:
		if len(wsInfos) == 0 {
			report.add("", "workspace configurations", "ok", fmt.Sprintf("none under %s", relCfgDir), "")
		} else {
			report.add("", "workspace configurations", "ok",
				fmt.Sprintf("%d configuration(s) under %s: %s", len(wsInfos), relCfgDir, infoNames(wsInfos)), "")
		}
		byName := make(map[string]*basev0.ConfigurationInformation)
		for _, info := range wsInfos {
			byName[info.Name] = info
		}
		for _, name := range required {
			info, ok := byName[name]
			if !ok {
				report.add(codeConfigurationMissing, "workspace configurations", "fail",
					fmt.Sprintf("required workspace configuration %q not found under %s (required by %s)", name, relCfgDir, strings.Join(requiredBy[name], ", ")),
					fmt.Sprintf("add %s/%s.env (or %s.secret.env holding provider references)", relCfgDir, name, name))
				continue
			}
			if len(info.ConfigurationValues) == 0 && info.Data == nil {
				report.add(codeConfigurationMissing, "workspace configurations", "fail",
					fmt.Sprintf("workspace configuration %q exists under %s but defines no values (required by %s)", name, relCfgDir, strings.Join(requiredBy[name], ", ")),
					fmt.Sprintf("populate the %s files for %q with the required keys", relCfgDir, name))
			}
		}
		if serviceScoped {
			// Resolve only what the selected service declares; unrelated
			// workspace configurations must not be touched.
			for _, name := range required {
				if info, ok := byName[name]; ok {
					toResolve = append(toResolve, scopedConfiguration{origin: "workspace", info: info})
				}
			}
		} else {
			for _, info := range wsInfos {
				toResolve = append(toResolve, scopedConfiguration{origin: "workspace", info: info})
			}
		}
	}

	serviceConfigurations := 0
	for _, svc := range scope {
		svcCfgDir := filepath.Join(svc.Dir(), "configurations", env.Name)
		label := fmt.Sprintf("service %s", serviceUnique(svc))
		relSvcDir := filepath.Join(svc.Name, "configurations", env.Name)
		if !dirExists(svcCfgDir) {
			if others := otherEnvironmentDirs(filepath.Join(svc.Dir(), "configurations"), env.Name); len(others) > 0 {
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
	return infos, true
}

// checkSecretReferences resolves every secret provider reference in scope
// through the environment's configured backend, in memory, and discards the
// values immediately. This is the behavior being diagnosed: a locked or
// unavailable provider must fail here, not halfway through `codefly run`.
func checkSecretReferences(ctx context.Context, env *resources.Environment, resolvers map[string]configurations.SecretResolver, unavailable map[string]bool, toResolve []scopedConfiguration, report *workspaceReadinessReport) {
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

func accountHint(env *resources.Environment) string {
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
   service?, status: ready|not_ready, checks: [{code, name, status, message,
   remediation?}]}

Stable diagnostic codes: workspace_not_found, workspace_invalid,
environment_not_found, service_not_found, module_reference_unresolved,
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
	case "fail":
		result.status = statusFail
		result.detail = fmt.Sprintf("%s [%s]", diagnostic.Message, diagnostic.Code)
	default:
		result.status = statusOK
	}
	result.print()
}

func init() {
	DoctorWorkspaceCmd.Flags().StringVar(&doctorWorkspaceEnv, "env", "local", "Environment to validate against")
	DoctorWorkspaceCmd.Flags().StringVar(&doctorWorkspaceService, "service", "", "Restrict validation to one service's declared configuration dependencies")
	DoctorWorkspaceCmd.Flags().BoolVar(&doctorWorkspaceJSON, "json", false, "Print a machine-readable report to stdout")
	DoctorWorkspaceCmd.Flags().DurationVar(&doctorWorkspaceTimeout, "timeout", 30*time.Second, "Overall bound; secret resolution is cancelled when it expires")
	DoctorCmd.AddCommand(DoctorWorkspaceCmd)
}
