// Package solutionrun derives what a composed solution needs at run time: the
// api.consumes projection its backend federates from, and the registration
// secrets the two ends of that exchange authenticate with.
//
// It lives under pkg so one derivation serves every run path. pkg/control
// depends only downward and never on cmd, so a derivation owned by the run
// command could not reach the lifecycles it drives.
package solutionrun

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"gopkg.in/yaml.v3"
)

// RootRef returns the workspace's own module reference — the `path: .`
// self module (or, failing an explicit path, the module whose name matches the
// workspace) — or nil if none is present.
//
// It is a tie-breaker, not a gate: a solution composed into a workspace by
// source and version is never the workspace's own module, yet it runs — and
// federates — exactly like one checked out at the root. What a run derives is
// therefore located by the module (see entryManifest); RootRef only decides
// between solution roots that nothing else tells apart (see SolutionRoots).
func RootRef(workspace *resources.Workspace) *resources.ModuleReference {
	for _, ref := range workspace.Modules {
		if ref.PathOverride != nil && *ref.PathOverride == "." {
			return ref
		}
	}
	for _, ref := range workspace.Modules {
		if ref.Name == workspace.Name {
			return ref
		}
	}
	return nil
}

// entryManifest returns the solution manifest of the module whose service-entry
// is service, or nil when service is not that entry or the module ships no
// manifest. The manifest sits beside the module manifest, in the module's own
// directory — the workspace root for a `path: .` module, a cache checkout for one
// composed by source and version. Locating it by the module rather than by the
// workspace is what lets a composed solution derive the same run inputs as a
// root one: gated on the workspace's own module, a composed solution booted with
// no projection and no credential, and its registration was refused by the host
// it was composed against.
//
// The gate on service being the module's entry is still what keeps one
// solution's manifest off another module's backend: the manifest describes the
// module it sits in, and only that module's entry runs it.
func entryManifest(module *resources.Module, service *resources.Service) (*manifest.Manifest, error) {
	if module == nil || service == nil || module.ServiceEntry == "" || module.ServiceEntry != service.Name {
		return nil, nil
	}
	return moduleManifest(module)
}

// moduleManifest returns the solution manifest a module ships, or nil when it
// ships none. A module with no directory — one constructed rather than loaded —
// has nowhere to ship it, so it is read as having none rather than as a relative
// path against the working directory.
func moduleManifest(module *resources.Module) (*manifest.Manifest, error) {
	if module.Dir() == "" {
		return nil, nil
	}
	return loadManifestForRun(module.Dir())
}

// loadManifestForRun decodes the solution manifest leniently: running a
// solution needs only the api.consumes projection, so a manifest carrying a
// field from a newer core — or tripping a schema rule unrelated to federation —
// must not make the solution unrunnable. manifest.Load's strict KnownFields and
// full Validate remain the gate for `sync` and `package`, which do consume the
// whole schema. Returns nil when the directory has no manifest.
func loadManifestForRun(moduleDir string) (*manifest.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(moduleDir, manifest.FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", manifest.FileName, err)
	}
	var solutionManifest manifest.Manifest
	if err := yaml.Unmarshal(data, &solutionManifest); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", manifest.FileName, err)
	}
	if err := validateConsumedBindings(solutionManifest.API.Consumes); err != nil {
		return nil, err
	}
	return &solutionManifest, nil
}

// registrationIdentityPattern is the shape the registrar requires of every
// identity it declares a digest for — a facade prefix and a solution id alike —
// mirrored here so a run fails on the manifest rather than at the registrar's
// startup. registrationIdentityMaxLength bounds it to one DNS label.
var registrationIdentityPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

const registrationIdentityMaxLength = 63

// isRegistrationIdentity reports whether the registrar would accept identity in
// a `identity:sha256hex` declaration.
func isRegistrationIdentity(identity string) bool {
	return registrationIdentityPattern.MatchString(identity) && len(identity) <= registrationIdentityMaxLength
}

// validateConsumedBindings rejects a partially bound api.consumes entry, a facade
// prefix claimed twice, and a prefix that is not a routing label. The lenient
// decode above skips manifest.Validate, so this is the only gate a run passes
// through, and every rule is one the run itself depends on.
//
// A partial bind — ConsumedAPIs() drops only entries with an empty module — would
// project into a CODEFLY__ENDPOINT key built from empty segments, which no
// runtime can resolve.
//
// A repeated prefix is worse than unroutable: one prefix mints one registration
// secret, so two entries sharing a prefix hand the services of two different
// modules the same credential, and either module can then mint the other's
// service-principal work context. manifest.Validate rejects a duplicated `as`
// for sync and package; a run must not be the path that turns the same typo into
// cross-module identity confusion.
//
// A prefix that is not a routing label is caught here because the run cannot
// encode it: every declaration this run derives is `prefix:value` joined on ",",
// and the registrar splits an entry on its first ":" before checking the prefix
// against this same shape. An `as` carrying a separator, an uppercase letter or
// an underscore therefore reaches the registrar as a malformed declaration, which
// it refuses at startup — so the whole composition fails to boot, naming a
// credential rather than the manifest field that is actually wrong.
func validateConsumedBindings(consumes []manifest.APIDeclaration) error {
	seenAs := make(map[string]string, len(consumes))
	for i := range consumes {
		declaration := &consumes[i]
		bound := 0
		for _, field := range []string{declaration.Module, declaration.Service, declaration.Endpoint} {
			if field != "" {
				bound++
			}
		}
		if bound != 0 && bound != 3 {
			return fmt.Errorf("%s: api.consumes entry %q binds only part of module/service/endpoint", manifest.FileName, declaration.ID)
		}
		if declaration.As == "" {
			continue
		}
		if !isRegistrationIdentity(declaration.As) {
			return fmt.Errorf("%s: api.consumes entry %q claims the facade prefix %q; a prefix is one routing label: lowercase letters, digits and dashes, starting and ending alphanumeric, at most %d characters", manifest.FileName, declaration.ID, declaration.As, registrationIdentityMaxLength)
		}
		if first, duplicate := seenAs[declaration.As]; duplicate {
			return fmt.Errorf("%s: api.consumes entries %q and %q both claim the facade prefix %q; one prefix is one identity", manifest.FileName, first, declaration.ID, declaration.As)
		}
		seenAs[declaration.As] = declaration.ID
	}
	return nil
}

// --- Module registration secret provisioning ---
//
// The gateway federates a consumed module's /v1/<prefix>/* only against a token
// accounts signed and bound to that prefix, and accounts issues one only to a
// caller presenting the secret whose digest the composition declared. Both
// halves are provisioned here, per run: the plaintext rides a process override
// to the consuming backend, and the digest is declared into the federation
// workspace configuration group, so it reaches the registrar on the carrier a
// service reads by contract. Provisioning writes nothing to disk: a secret lives
// in the environment of the processes that spend it and never outlives the run —
// unless an operator asks for it, since --output-env exports a service's whole
// runtime environment, overrides included, to an owner-only file.
//
// A prefix gets two independent secrets, because two different principals
// authenticate under it. The consuming backend registers the route with the
// registration secret; the consumed module presents the identity secret to mint
// the service-principal work context every module-facing RPC is authenticated
// from. Each is declared under its own key, so the registrar can tell a backend
// registering "documents" from the service principal of "documents" — with one
// secret it could not, and a backend could mint the work context of every module
// it consumes.

const (
	// federationConfigurationGroup is the workspace-configuration group a host
	// declares to receive the federation policy. Naming the group rather than a
	// service keeps this free of any particular host's service names — whichever
	// service depends on it is the registrar.
	federationConfigurationGroup = "federation"
	// moduleRegistrationSecretsKey is the key inside that group carrying the
	// `prefix:sha256hex` digests of the secrets the consuming backend registers
	// with.
	// #nosec G101 -- a configuration key name, not a credential
	moduleRegistrationSecretsKey = "MODULE_REGISTRATION_SECRETS"
	// moduleIdentitySecretsKey carries the digests of the secrets the consumed
	// modules themselves present, in the same encoding. A separate key is what
	// makes the two exchanges separable at all: the registrar resolves one digest
	// per prefix per key, so a single key can only describe a single principal.
	//
	// Unlike the key above, this one is asserted by the CLI rather than read from
	// a host contract that predates it: a registrar that does not yet consult it
	// authenticates a module's work-context exchange against the registration
	// digest instead, and fails it closed. The run cannot detect that — a host
	// reads the group from its own code, and declaring a key is neither necessary
	// nor sufficient for reading it — so the two repositories are pinned to each
	// other by release order, not by a runtime check.
	// #nosec G101 -- a configuration key name, not a credential
	moduleIdentitySecretsKey = "MODULE_IDENTITY_SECRETS"
	// moduleRegistrationSecretsEnvironmentVariable carries the plaintext
	// `prefix:secret` twin to the consuming backend. It is a wire contract with
	// the solution runtimes, which read it verbatim; its natural home is core's
	// solution/manifest, next to APIConsumesEnvironmentVariable, once a core
	// release carries it.
	// #nosec G101 -- an environment variable name, not a credential
	moduleRegistrationSecretsEnvironmentVariable = "CODEFLY__MODULE_REGISTRATION_SECRETS"
	// moduleIdentityPrefixEnvironmentVariable identifies the consumed module by
	// its declared federation prefix, which need not equal its module name.
	moduleIdentityPrefixEnvironmentVariable = "CODEFLY__MODULE_IDENTITY_PREFIX"
	// moduleIdentitySecretEnvironmentVariable carries only that module's identity
	// secret, never the registration secret or its siblings' credentials.
	// #nosec G101 -- an environment variable name, not a credential
	moduleIdentitySecretEnvironmentVariable = "CODEFLY__MODULE_IDENTITY_SECRET"
	// moduleRegistrationSecretEnvironmentVariable is a deprecated identity-secret
	// alias for existing module runtimes. Despite its name it must carry the same
	// identity secret as the canonical carrier, never the registration secret.
	// #nosec G101 -- an environment variable name, not a credential
	moduleRegistrationSecretEnvironmentVariable = "CODEFLY__MODULE_REGISTRATION_SECRET"
	// moduleRegistrationSecretBytes is the entropy of one generated secret. It is
	// hex-encoded, so a secret can never contain the "," or ":" that separate
	// entries on either side of the exchange.
	moduleRegistrationSecretBytes = 32

	// solutionRegistrationSecretsKey carries, in the same `id:sha256hex`
	// encoding, the digests of the secrets solutions register with. The host
	// declares it apart from the module keys on purpose: a solution registers a
	// frontend remote that executes in the host origin with the viewer's
	// credentials, strictly more authority than federating a REST prefix, so
	// neither credential may stand in for the other. The registrar reads this key
	// on every exchange rather than at startup, so a solution mounting against a
	// host already serving is admitted without restarting it.
	// #nosec G101 -- a configuration key name, not a credential
	solutionRegistrationSecretsKey = "SOLUTION_REGISTRATION_SECRETS"
	// solutionRegistrationSecretEnvironmentVariable carries the plaintext twin to
	// the solution's entry service. The solution runtimes read it as an explicit
	// override of the `solution-registration/SECRET` workspace secret, which is
	// the declared carrier for a deployed solution; a run mints the secret per
	// process and has no store to declare it in, so the override is its carrier.
	// #nosec G101 -- an environment variable name, not a credential
	solutionRegistrationSecretEnvironmentVariable = "CODEFLY__SOLUTION_REGISTRATION_SECRET"
)

// moduleRegistrationSecrets is one run's provisioning: the secrets each federated
// prefix carries. The encodings the two ends and the registrar read are derived
// from these, so the plaintext a principal presents and the digest it is checked
// against cannot drift apart.
type moduleRegistrationSecrets struct {
	// prefixes are the facade prefixes provisioned, in the order every encoding
	// lists them.
	prefixes []string
	byPrefix map[string]modulePrefixSecrets
}

// modulePrefixSecrets is what one prefix carries: the secret the consuming
// backend registers the route with, and the secret the consumed module proves its
// own identity with. They are independent values, so holding one grants nothing
// about the other.
type modulePrefixSecrets struct {
	registration string
	identity     string
}

// registrationSecrets is the plaintext the consuming backend presents to register
// every prefix it consumes.
func (provisioned *moduleRegistrationSecrets) registrationSecrets() string {
	return provisioned.encode(func(secrets modulePrefixSecrets) string { return secrets.registration })
}

// registrationDigests is what the registrar checks a registering backend against.
func (provisioned *moduleRegistrationSecrets) registrationDigests() string {
	return provisioned.encode(func(secrets modulePrefixSecrets) string { return moduleSecretDigest(secrets.registration) })
}

// identityDigests is what the registrar checks a module's own work-context
// exchange against. The identity plaintext is never encoded as a map: no principal
// holds more than its own.
func (provisioned *moduleRegistrationSecrets) identityDigests() string {
	return provisioned.encode(func(secrets modulePrefixSecrets) string { return moduleSecretDigest(secrets.identity) })
}

// encode projects one value per prefix into the `prefix:value,…` form both sides
// of the exchange parse, in the order prefixes lists them.
func (provisioned *moduleRegistrationSecrets) encode(value func(modulePrefixSecrets) string) string {
	entries := make([]string, 0, len(provisioned.prefixes))
	for _, prefix := range provisioned.prefixes {
		entries = append(entries, prefix+":"+value(provisioned.byPrefix[prefix]))
	}
	return strings.Join(entries, ",")
}

// provisionModuleRegistrationSecrets mints a fresh secret for each distinct
// facade prefix among the consumed APIs. An entry with no facade entry-point is
// skipped: the runtime refuses to guess a prefix for it, so it will never
// register and a secret for it would authorize nothing.
func provisionModuleRegistrationSecrets(consumed []manifest.ConsumedAPI) *moduleRegistrationSecrets {
	prefixes := make([]string, 0, len(consumed))
	for i := range consumed {
		prefix := consumed[i].As
		// Distinct prefixes only: two entries sharing a facade are one federated
		// route, and a repeated prefix is a declaration error the registrar
		// rejects outright.
		if prefix == "" || slices.Contains(prefixes, prefix) {
			continue
		}
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) == 0 {
		return nil
	}

	byPrefix := make(map[string]modulePrefixSecrets, len(prefixes))
	for _, prefix := range prefixes {
		byPrefix[prefix] = modulePrefixSecrets{registration: mintModuleSecret(), identity: mintModuleSecret()}
	}
	return &moduleRegistrationSecrets{prefixes: prefixes, byPrefix: byPrefix}
}

func mintModuleSecret() string {
	raw := make([]byte, moduleRegistrationSecretBytes)
	// crypto/rand.Read fills the buffer completely or crashes the program; it
	// cannot return a short read.
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// moduleSecretDigest is how the registrar recognizes a presented secret.
func moduleSecretDigest(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

// federationRegistrars returns the module-qualified uniques of the services
// declaring a dependency on the federation configuration group — the services
// that decide whether a module may claim a prefix. The declared digests reach
// them through that group, so this answers only whether provisioning a run
// credential can authorize anything at all, and names them for the run log.
//
// Modules and services that fail to load are skipped rather than failing the
// run: this walks the whole workspace, including composed modules that may not
// resolve locally, and a module the flow cannot load is not one it can run
// either. A registrar that never appears simply leaves federation unconfigured,
// which the caller reports.
func federationRegistrars(ctx context.Context, workspace *resources.Workspace) []string {
	var registrars []string
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			continue
		}
		for _, svcRef := range mod.ServiceReferences {
			svc, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: svcRef.Name, Module: mod.Name})
			if err != nil {
				continue
			}
			if slices.Contains(svc.WorkspaceConfigurationDependencies, federationConfigurationGroup) {
				registrars = append(registrars, resources.ServiceUnique(mod.Name, svc.Name))
			}
		}
	}
	return registrars
}

// consumedModuleSecretInjection is what one run derives for the modules it
// consumes: the overrides to apply, and the modules left out, kept apart by why.
// The caller reports all three — a module that silently receives no secret is
// indistinguishable at runtime from one whose exchange is broken, which is the
// diagnosis this provisioning exists to end.
type consumedModuleSecretInjection struct {
	overrides map[string]map[string]string
	// provisioned are the modules whose services received their own secret.
	provisioned []string
	// registrars are consumed modules that hold the digests themselves, and so
	// are deliberately left out.
	registrars []string
	// unresolved are consumed modules with no service to inject into, each with
	// the reason it has none.
	unresolved []string
}

// consumedModuleSecretOverrides maps every service of each consumed module to
// the secret minted for the prefix that module federates under, keyed by the
// module-qualified unique so an injection lands on exactly one service.
//
// A module holding a federation registrar is excluded: it is the authority the
// exchange runs against, not a module that authenticates to it — it mints work
// contexts rather than presenting a secret for one. Injecting there would put the
// plaintext and the digest it is checked against in the same process, dissolving
// the separation the digest carrier exists to create.
func consumedModuleSecretOverrides(ctx context.Context, workspace *resources.Workspace, consumed []manifest.ConsumedAPI, provisioned *moduleRegistrationSecrets, registrars []string) consumedModuleSecretInjection {
	injection := consumedModuleSecretInjection{overrides: make(map[string]map[string]string)}
	holdsDigests := registrarModules(registrars)
	for _, binding := range consumedModuleBindings(consumed) {
		if slices.Contains(holdsDigests, binding.module) {
			injection.registrars = append(injection.registrars, binding.module)
			continue
		}
		services, err := moduleServiceUniques(ctx, workspace, binding.module)
		if err != nil {
			injection.unresolved = append(injection.unresolved, fmt.Sprintf("%s (%v)", binding.module, err))
			continue
		}
		if len(services) == 0 {
			injection.unresolved = append(injection.unresolved, fmt.Sprintf("%s (declares no service)", binding.module))
			continue
		}
		injection.provisioned = append(injection.provisioned, binding.module)
		for _, unique := range services {
			injection.overrides[unique] = map[string]string{
				moduleIdentityPrefixEnvironmentVariable:     binding.prefix,
				moduleIdentitySecretEnvironmentVariable:     provisioned.byPrefix[binding.prefix].identity,
				moduleRegistrationSecretEnvironmentVariable: provisioned.byPrefix[binding.prefix].identity,
			}
		}
	}
	return injection
}

// consumedModuleBinding is the single prefix a consumed module federates under —
// that module's identity for this run.
type consumedModuleBinding struct {
	module string
	prefix string
}

// consumedModuleBindings pairs each consumed module with the prefix whose secret
// is its identity, in declaration order, once per module.
//
// An entry with no facade prefix minted no secret, so it binds nothing and must
// not be what a module is remembered by: the prefix test therefore precedes the
// per-module one, leaving a module declared both with and without a prefix bound
// to the prefix that actually has a secret. validateConsumedBindings rejects a
// prefix claimed twice, so no prefix reaches two modules; a module declared under
// several prefixes keeps the first, holding one identity either way.
func consumedModuleBindings(consumed []manifest.ConsumedAPI) []consumedModuleBinding {
	var bindings []consumedModuleBinding
	for i := range consumed {
		module, prefix := consumed[i].Module, consumed[i].As
		if prefix == "" {
			continue
		}
		if slices.ContainsFunc(bindings, func(bound consumedModuleBinding) bool { return bound.module == module }) {
			continue
		}
		bindings = append(bindings, consumedModuleBinding{module: module, prefix: prefix})
	}
	return bindings
}

// registrarModules reduces the registrar service uniques to the modules holding
// them.
func registrarModules(registrars []string) []string {
	modules := make([]string, 0, len(registrars))
	for _, unique := range registrars {
		module, _, _ := strings.Cut(unique, "/")
		modules = append(modules, module)
	}
	return modules
}

// moduleServiceUniques returns the module-qualified uniques of every service the
// named module declares.
//
// Neither failure is folded into an empty result. A module the workspace does not
// reference and a module whose checkout will not load leave the same hole in the
// federation — the consumer idles with a valid-looking run — so each is returned
// as the reason it left one, for the caller to report. Loading is still not fatal
// to the run: a composed module that does not resolve locally is not one this run
// can start either, and the solution still serves its own routes.
func moduleServiceUniques(ctx context.Context, workspace *resources.Workspace, module string) ([]string, error) {
	for _, ref := range workspace.Modules {
		if ref.Name != module {
			continue
		}
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("cannot load module: %w", err)
		}
		uniques := make([]string, 0, len(mod.ServiceReferences))
		for _, svcRef := range mod.ServiceReferences {
			uniques = append(uniques, resources.ServiceUnique(mod.Name, svcRef.Name))
		}
		return uniques, nil
	}
	return nil, fmt.Errorf("workspace <%s> references no such module", workspace.Name)
}

// RunInputs are the two carriers a run derives for a solution: per-service
// process overrides, and values for the workspace configuration groups a service
// declares. Returned together (never assigned to a global from inside) so a run
// that derives nothing clears both, and a second in-process invocation cannot
// inherit the previous run's values.
type RunInputs struct {
	Overrides               map[string]map[string]string
	WorkspaceConfigurations map[string]map[string]string
	// Notes is what the derivation wants an operator told, in order.
	Notes []Note
}

// Note is one line of narration, returned rather than printed. This package is
// called both by the run command, which owns a terminal, and by the control
// plane, which runs inside a process whose stdout is a JSON-RPC stream — the
// MCP server serves on it — where a narration line corrupts the protocol. Only
// a caller that knows it owns a terminal can decide to render these.
type Note struct {
	// Warning marks a line an operator has to act on — a federation that cannot
	// work — rather than a statement of what was supplied.
	Warning bool
	Message string
}

// DerivedRunInputs resolves the solution injections for the service being run,
// when it is the service-entry of a module shipping a solution manifest: the
// CODEFLY__API_CONSUMES projection, the registration secrets that let the
// consuming backend prove which module it is, and the registration secret the
// solution itself registers with the host under. Every run path derives them
// here so `run service <entry>`, `run solution` and the control plane inject
// identically, and it reports what it sent through Notes: the values ride Start
// overrides, which each service agent chooses to honor, so an operator debugging
// dead federation must be able to see that the CLI supplied them before
// suspecting the manifest.
//
// Nothing here depends on the module being the workspace's own: a solution
// composed by source and version derives the same inputs from the manifest in
// its cache checkout.
func DerivedRunInputs(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, serviceName string) (RunInputs, error) {
	solutionManifest, err := entryManifest(module, service)
	if err != nil {
		return RunInputs{}, err
	}
	if solutionManifest == nil {
		return RunInputs{}, nil
	}
	var notes []Note
	overrides := map[string]map[string]string{serviceName: {}}
	consumed := solutionManifest.ConsumedAPIs()
	if len(consumed) > 0 {
		ids := make([]string, 0, len(consumed))
		for i := range consumed {
			ids = append(ids, consumed[i].ID)
		}
		overrides[serviceName][manifest.APIConsumesEnvironmentVariable] = solutionManifest.ConsumedAPIsEnvValue()
		notes = append(notes, Note{Message: fmt.Sprintf("injecting %s into %s: %s",
			manifest.APIConsumesEnvironmentVariable, serviceName, strings.Join(ids, ", "))})
	}

	provisioned := provisionModuleRegistrationSecrets(consumed)
	registrars := federationRegistrars(ctx, workspace)
	if len(registrars) == 0 {
		// Without a registrar holding the digests, nothing can authorize a mint.
		// Hand the backend a secret anyway and it spends every heartbeat on an
		// exchange that cannot succeed; withholding it lets the runtime skip the
		// exchange with the accurate "no registration secret provisioned" line
		// instead. That is a composition gap, not a reason to refuse to run, so
		// say so and boot: the solution still serves its own routes.
		withheld := []string{"solution " + module.Name + " cannot register"}
		if provisioned != nil {
			withheld = append(withheld, fmt.Sprintf("consumed modules (%s) cannot federate", strings.Join(provisioned.prefixes, ", ")))
		}
		notes = append(notes, Note{Warning: true, Message: fmt.Sprintf(
			"no service declares the %q workspace configuration: %s",
			federationConfigurationGroup, strings.Join(withheld, "; "))})
		return RunInputs{Overrides: mergeOverrides(overrides), Notes: notes}, nil
	}
	declared := map[string]string{}

	// The solution's own credential: the host admits a solution's gateway upstream
	// and its frontend remote only against the digest declared for the id it
	// registers under, which is the module's name. Minted per run, like the
	// module secrets, so nothing written to disk can register in the next run.
	solutionSecret, solutionNotes := provisionSolutionRegistrationSecret(module.Name, serviceName, registrars)
	notes = append(notes, solutionNotes...)
	if solutionSecret != "" {
		overrides[serviceName][solutionRegistrationSecretEnvironmentVariable] = solutionSecret
		declared[solutionRegistrationSecretsKey] = module.Name + ":" + moduleSecretDigest(solutionSecret)
	}

	if provisioned == nil {
		return RunInputs{
			Overrides:               mergeOverrides(overrides),
			Notes:                   notes,
			WorkspaceConfigurations: federationDeclaration(declared),
		}, nil
	}
	overrides[serviceName][moduleRegistrationSecretsEnvironmentVariable] = provisioned.registrationSecrets()
	notes = append(notes, Note{Message: fmt.Sprintf(
		"provisioned registration secrets for %s into %s, registration and identity digests into %s",
		strings.Join(provisioned.prefixes, ", "), serviceName, strings.Join(registrars, ", "))})

	// The consuming backend is only one end of the exchange: a consumed module
	// presents its own identity secret to mint the service-principal work context,
	// without which every module-facing RPC it makes is unauthenticated and its
	// background workers idle. Every module is accounted for out loud — the line
	// above otherwise reads as a fully wired federation while half of it is
	// missing, which is the diagnosis this provisioning exists to end.
	injection := consumedModuleSecretOverrides(ctx, workspace, consumed, provisioned, registrars)
	if len(injection.provisioned) > 0 {
		notes = append(notes, Note{Message: fmt.Sprintf("provisioned %s and %s into the services of %s",
			moduleIdentityPrefixEnvironmentVariable, moduleIdentitySecretEnvironmentVariable, strings.Join(injection.provisioned, ", "))})
	}
	if len(injection.registrars) > 0 {
		notes = append(notes, Note{Message: fmt.Sprintf(
			"consumed modules %s declare the %q group and hold the digests: they mint work contexts rather than present a secret for one",
			strings.Join(injection.registrars, ", "), federationConfigurationGroup)})
	}
	if len(injection.unresolved) > 0 {
		notes = append(notes, Note{Warning: true, Message: fmt.Sprintf(
			"no %s provisioned for %s: those modules cannot obtain a work context, so their module-facing workers will idle",
			moduleIdentitySecretEnvironmentVariable, strings.Join(injection.unresolved, "; "))})
	}
	declared[moduleRegistrationSecretsKey] = provisioned.registrationDigests()
	declared[moduleIdentitySecretsKey] = provisioned.identityDigests()
	return RunInputs{
		Overrides:               mergeOverrides(overrides, injection.overrides),
		Notes:                   notes,
		WorkspaceConfigurations: federationDeclaration(declared),
	}, nil
}

// federationDeclaration wraps the digests a run declares into the federation
// group, or nil when it declares none, so a run that provisioned nothing sets no
// configuration at all.
func federationDeclaration(declared map[string]string) map[string]map[string]string {
	if len(declared) == 0 {
		return nil
	}
	return map[string]map[string]string{federationConfigurationGroup: declared}
}

// provisionSolutionRegistrationSecret mints the secret the solution named id
// registers with, and says what it did. It returns no secret, with a warning,
// when the registrar could not hold a digest for id — the registrar refuses a
// malformed declaration at startup, so declaring one would take the whole host
// down with a message blaming a credential — and when the solution's own module
// is the registrar: that module holds the digests and injecting the preimage
// beside them dissolves the separation the digest carrier exists to create.
func provisionSolutionRegistrationSecret(id, serviceName string, registrars []string) (string, []Note) {
	if slices.Contains(registrarModules(registrars), id) {
		return "", []Note{{Message: fmt.Sprintf(
			"module %s declares the %q group and holds the digests: it admits solutions rather than registers as one", id, federationConfigurationGroup)}}
	}
	if !isRegistrationIdentity(id) {
		return "", []Note{{Warning: true, Message: fmt.Sprintf(
			"no %s provisioned: the module name %q is not an identity the registrar accepts (lowercase letters, digits and dashes, starting and ending alphanumeric, at most %d characters), so solution %s cannot register",
			solutionRegistrationSecretEnvironmentVariable, id, registrationIdentityMaxLength, id)}}
	}
	secret := mintModuleSecret()
	return secret, []Note{{Message: fmt.Sprintf(
		"provisioned %s for solution %s into %s, its digest into %s",
		solutionRegistrationSecretEnvironmentVariable, id, serviceName, strings.Join(registrars, ", "))}}
}

// Merge layers the inputs derived for one root of a run over those derived for
// another. Overrides merge key by key, later winning, exactly as mergeOverrides
// does. The workspace configuration values merge differently: every value this
// package declares is a comma-separated list of `identity:sha256hex` entries,
// and two solution roots in one run each declare their own — the second must
// join the first, not replace it, or only the last-named solution can register.
// Notes keep their order across roots.
func Merge(base, layer RunInputs) RunInputs {
	merged := RunInputs{
		Overrides: mergeOverrides(base.Overrides, layer.Overrides),
		Notes:     append(append([]Note{}, base.Notes...), layer.Notes...),
	}
	if len(base.WorkspaceConfigurations) == 0 && len(layer.WorkspaceConfigurations) == 0 {
		return merged
	}
	merged.WorkspaceConfigurations = make(map[string]map[string]string)
	for _, source := range []map[string]map[string]string{base.WorkspaceConfigurations, layer.WorkspaceConfigurations} {
		for group, values := range source {
			if merged.WorkspaceConfigurations[group] == nil {
				merged.WorkspaceConfigurations[group] = make(map[string]string, len(values))
			}
			for key, value := range values {
				merged.WorkspaceConfigurations[group][key] = joinDeclarations(merged.WorkspaceConfigurations[group][key], value)
			}
		}
	}
	return merged
}

// joinDeclarations appends the `identity:value` entries of layer to those of
// base, keeping the first declaration of an identity. The registrar refuses a
// declaration naming one identity twice — and boots nothing when it does — so
// two roots claiming one identity must not produce a duplicate; the first named
// root keeps it, as it did when the later value simply replaced the earlier one.
func joinDeclarations(base, layer string) string {
	if base == "" {
		return layer
	}
	entries := strings.Split(base, ",")
	for _, entry := range strings.Split(layer, ",") {
		if entry == "" {
			continue
		}
		identity, _, _ := strings.Cut(entry, ":")
		if slices.ContainsFunc(entries, func(existing string) bool {
			declared, _, _ := strings.Cut(existing, ":")
			return declared == identity
		}) {
			continue
		}
		entries = append(entries, entry)
	}
	return strings.Join(entries, ",")
}

// mergeOverrides layers per-service override maps, later layers winning key by
// key. Returns nil when nothing is set, so a flow with no overrides is
// indistinguishable from one that never had any; a service that received no
// value is likewise left out rather than listed with nothing.
func mergeOverrides(layers ...map[string]map[string]string) map[string]map[string]string {
	merged := make(map[string]map[string]string)
	for _, layer := range layers {
		for service, values := range layer {
			if len(values) == 0 {
				continue
			}
			if merged[service] == nil {
				merged[service] = make(map[string]string, len(values))
			}
			maps.Copy(merged[service], values)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}
