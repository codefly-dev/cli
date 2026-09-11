package run

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// SolutionCmd boots a solution as a unit from its root. A solution root is a
// workspace whose module declares a service-entry — the single runnable service
// the whole composition hangs off. Rather than a second sequencing engine, this
// resolves that entry and delegates to the same dependency-graph orchestration
// as `run service <entry>`, so a solution and its entry service boot identically.
var SolutionCmd = &cobra.Command{
	Use:   "solution",
	Short: "Start a solution locally: boot its service-entry with the full dependency graph",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			done()
			return fmt.Errorf("cannot load workspace: %w", err)
		}
		// Pull composed modules that resolve to a pinned artifact into the local
		// cache and point the overlay at them, so the delegated run below loads
		// them as local checkouts instead of erroring on an unfetched coordinate.
		if err = materializePinnedModules(ctx, workspace); err != nil {
			done()
			return err
		}
		// Reload so the overlay materialize just wrote is in effect: composed
		// pinned modules now resolve to their cache checkout, which the
		// service-entry scan may need to load.
		workspace, err = common.LoadWorkspace(ctx)
		if err != nil {
			done()
			return fmt.Errorf("cannot reload workspace: %w", err)
		}
		entry, err := resolveSolutionEntry(ctx, workspace)
		done()
		if err != nil {
			return err
		}
		// Delegate to the run-service path with the resolved entry. It reloads
		// the workspace and boots the full dependency graph — reusing every run
		// flag default seeded by ServiceCmd's init, plus the solution-facing
		// flags registered below (which bind the same package vars).
		return runServiceCommand(cmd, []string{entry})
	},
}

// resolveSolutionEntry finds the solution root and returns its
// "<module>/<service-entry>" unique. The root is the workspace's own module —
// the one referenced by `path: .` (equivalently, whose name matches the
// workspace). Composed dependency modules (e.g. the saas host) may declare their
// own service-entry, but those are dependencies, not the solution root, so they
// must not be treated as competing roots.
//
// When no self-root module is identifiable, fall back to scanning for a single
// module that declares a service-entry. Composed modules that fail to resolve
// (e.g. a pinned coordinate with no local checkout yet) are not the local root,
// so their load errors are collected and only surfaced if no entry is found.
func resolveSolutionEntry(ctx context.Context, workspace *resources.Workspace) (string, error) {
	if root := solutionRootRef(workspace); root != nil {
		mod, err := workspace.LoadModuleFromReference(ctx, root)
		if err != nil {
			return "", fmt.Errorf("cannot load solution root module <%s>: %w", root.Name, err)
		}
		if mod.ServiceEntry == "" {
			return "", fmt.Errorf("solution root module <%s> declares no service-entry", mod.Name)
		}
		return mod.Name + "/" + mod.ServiceEntry, nil
	}

	var entries []string
	var loadErrs []error
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %w", ref.Name, err))
			continue
		}
		if mod.ServiceEntry != "" {
			entries = append(entries, mod.Name+"/"+mod.ServiceEntry)
		}
	}
	switch len(entries) {
	case 1:
		return entries[0], nil
	case 0:
		if len(loadErrs) > 0 {
			return "", fmt.Errorf("no solution root in workspace <%s>: no resolvable module declares a service-entry (some modules failed to resolve: %w)", workspace.Name, errors.Join(loadErrs...))
		}
		return "", fmt.Errorf("no solution root in workspace <%s>: no module declares a service-entry", workspace.Name)
	default:
		return "", fmt.Errorf("ambiguous solution root in workspace <%s>: multiple modules declare a service-entry (%s); run `codefly run service <module/service>` explicitly", workspace.Name, strings.Join(entries, ", "))
	}
}

// solutionRootRef returns the workspace's own module reference — the `path: .`
// self module (or, failing an explicit path, the module whose name matches the
// workspace) — or nil if none is present.
func solutionRootRef(workspace *resources.Workspace) *resources.ModuleReference {
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

// solutionEntryConsumes returns the solution's api.consumes projection when
// service is the solution root's own service-entry, along with the
// CODEFLY__API_CONSUMES value carrying it. The solution runtime reads that
// variable to register each consumed module's upstream with the gateway, so
// without it every consumed route stays unrouted.
//
// The manifest at the workspace root describes the workspace's own module, so
// the injection is gated on service belonging to that module: when no self-root
// module exists, resolveSolutionEntry falls back to scanning composed modules,
// and pairing this manifest with a composed module's service would bind one
// solution's consumes to another's backend.
func solutionEntryConsumes(workspace *resources.Workspace, module *resources.Module, service *resources.Service) ([]manifest.ConsumedAPI, string, error) {
	root := solutionRootRef(workspace)
	if root == nil || module == nil || service == nil {
		return nil, "", nil
	}
	if root.Name != module.Name || module.ServiceEntry != service.Name {
		return nil, "", nil
	}
	solutionManifest, err := loadSolutionManifestForRun(workspace.Dir())
	if err != nil || solutionManifest == nil {
		return nil, "", err
	}
	consumed := solutionManifest.ConsumedAPIs()
	if len(consumed) == 0 {
		return nil, "", nil
	}
	return consumed, solutionManifest.ConsumedAPIsEnvValue(), nil
}

// loadSolutionManifestForRun decodes the solution manifest leniently: running a
// solution needs only the api.consumes projection, so a manifest carrying a
// field from a newer core — or tripping a schema rule unrelated to federation —
// must not make the solution unrunnable. manifest.Load's strict KnownFields and
// full Validate remain the gate for `sync` and `package`, which do consume the
// whole schema. Returns nil when the workspace has no manifest.
func loadSolutionManifestForRun(workspaceDir string) (*manifest.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(workspaceDir, manifest.FileName))
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

// validateConsumedBindings rejects a partially bound api.consumes entry. The
// lenient decode above skips manifest.Validate, and ConsumedAPIs() drops only
// entries with an empty module — so an entry naming a module but no service or
// endpoint would project into a CODEFLY__ENDPOINT key built from empty
// segments, which no runtime can resolve.
func validateConsumedBindings(consumes []manifest.APIDeclaration) error {
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
	}
	return nil
}

func init() {
	// Solution-facing subset of the run flags, bound to the same package vars
	// runServiceCommand reads. The advanced/testing flags (cli-server, …) stay
	// ServiceCmd-only.
	SolutionCmd.Flags().StringVar(&fixture, "fixture", "", "Fixture override (defaults to the selected Codefly environment)")
	SolutionCmd.Flags().StringVar(&environmentName, "env", orchestration.LocalEnvironmentName, "Workspace environment to run")
	SolutionCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY, e.g. MCP, CI, pipes)")
	SolutionCmd.Flags().StringVar(&profile, "profile", "", "Named workspace run profile")
	SolutionCmd.Flags().StringSliceVar(&excludeDependencies, "exclude-dependency", nil, "Exclude optional dependency services from the run (repeatable, e.g. infra/temporal)")
	SolutionCmd.Flags().StringSliceVar(&setOverrides, "set", nil, "Per-service runtime env override (repeatable), e.g. --set warden:CODEFLY__FIXTURE=dogfood")
	SolutionCmd.Flags().StringSliceVar(&silent, "silent", nil, "Silence services in CLI output")
	// Port-isolation flags: fold a scope into every port hash so the whole
	// solution boots on a disjoint port set, in parallel with another running
	// stack. runServiceCommand reads cmd.Flags().Changed("naming-scope"), so an
	// explicit empty scope still clears a workspace-declared one here too. Same
	// usage text as ServiceCmd — one mechanism, one description.
	SolutionCmd.Flags().StringVar(&namingScope, "naming-scope", "", namingScopeUsage)
	SolutionCmd.Flags().BoolVar(&temporaryPorts, "temporary-ports", false, temporaryPortsUsage)
}

// --- Module registration secret provisioning ---
//
// The gateway federates a consumed module's /v1/<prefix>/* only against a token
// accounts signed and bound to that prefix, and accounts issues one only to a
// caller presenting the secret whose digest the composition declared. Both
// halves are provisioned here, per run: the plaintext rides a process override
// to the consuming backend, and the digest is declared into the federation
// workspace configuration group, so it reaches the registrar on the carrier a
// service reads by contract. Nothing is written to disk, so the raw secret
// exists only in the backend's process environment and never outlives the run.
//
// The same secret is the consumed module's own credential: a module presents it
// to the gateway to mint the service-principal work context every module-facing
// RPC is authenticated from, so its services get the plaintext too — one secret,
// spent by both ends of the exchange against the one digest the registrar holds.

const (
	// federationConfigurationGroup is the workspace-configuration group a host
	// declares to receive the federation policy. Naming the group rather than a
	// service keeps this free of any particular host's service names — whichever
	// service depends on it is the registrar.
	federationConfigurationGroup = "federation"
	// moduleRegistrationSecretsKey is the key inside that group carrying the
	// `prefix:sha256hex` digests this run declares.
	// #nosec G101 -- a configuration key name, not a credential
	moduleRegistrationSecretsKey = "MODULE_REGISTRATION_SECRETS"
	// moduleRegistrationSecretsEnvironmentVariable carries the plaintext
	// `prefix:secret` twin to the consuming backend. It is a wire contract with
	// the solution runtimes, which read it verbatim; its natural home is core's
	// solution/manifest, next to APIConsumesEnvironmentVariable, once a core
	// release carries it.
	// #nosec G101 -- an environment variable name, not a credential
	moduleRegistrationSecretsEnvironmentVariable = "CODEFLY__MODULE_REGISTRATION_SECRETS"
	// moduleRegistrationSecretEnvironmentVariable carries a consumed module's own
	// secret to that module's services. Singular: a module holds one identity, so
	// it needs only the entry minted for its own prefix — never the whole map,
	// which would hand every consumed module the credentials of its siblings.
	// #nosec G101 -- an environment variable name, not a credential
	moduleRegistrationSecretEnvironmentVariable = "CODEFLY__MODULE_REGISTRATION_SECRET"
	// moduleRegistrationSecretBytes is the entropy of one generated secret. It is
	// hex-encoded, so a secret can never contain the "," or ":" that separate
	// entries on either side of the exchange.
	moduleRegistrationSecretBytes = 32
)

// moduleRegistrationSecrets is one run's provisioning: the plaintext entries the
// consuming backend presents, and the digest entries the registrar compares them
// against.
type moduleRegistrationSecrets struct {
	// prefixes are the facade prefixes provisioned, in the order both encodings
	// list them.
	prefixes []string
	// byPrefix is the plaintext of each prefix, for the injection that hands one
	// module its own secret rather than the whole map.
	byPrefix map[string]string
	secrets  string
	digests  string
}

// provisionModuleRegistrationSecrets mints a fresh secret for each distinct
// facade prefix among the consumed APIs. An entry with no facade entry-point is
// skipped: the runtime refuses to guess a prefix for it, so it will never
// register and a secret for it would authorize nothing.
func provisionModuleRegistrationSecrets(consumed []manifest.ConsumedAPI) (*moduleRegistrationSecrets, error) {
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
		return nil, nil
	}

	secrets := make([]string, 0, len(prefixes))
	digests := make([]string, 0, len(prefixes))
	byPrefix := make(map[string]string, len(prefixes))
	for _, prefix := range prefixes {
		raw := make([]byte, moduleRegistrationSecretBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("cannot generate a registration secret for %q: %w", prefix, err)
		}
		secret := hex.EncodeToString(raw)
		digest := sha256.Sum256([]byte(secret))
		secrets = append(secrets, prefix+":"+secret)
		digests = append(digests, prefix+":"+hex.EncodeToString(digest[:]))
		byPrefix[prefix] = secret
	}
	return &moduleRegistrationSecrets{
		prefixes: prefixes,
		byPrefix: byPrefix,
		secrets:  strings.Join(secrets, ","),
		digests:  strings.Join(digests, ","),
	}, nil
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

// consumedModuleSecretOverrides maps every service of each consumed module to
// the secret minted for the prefix that module is federated under, keyed by the
// module-qualified unique so an injection lands on exactly one service. It also
// returns the modules provisioned, for the run log.
//
// A module the workspace cannot load contributes nothing: a module the flow
// cannot load is not one it can run either, so there is no service to inject
// into. A module consumed under several prefixes still holds one identity, and
// takes the secret of the first prefix it is declared under.
func consumedModuleSecretOverrides(ctx context.Context, workspace *resources.Workspace, consumed []manifest.ConsumedAPI, provisioned *moduleRegistrationSecrets) (map[string]map[string]string, []string) {
	overrides := make(map[string]map[string]string)
	var provisionedModules []string
	var seen []string
	for i := range consumed {
		module, prefix := consumed[i].Module, consumed[i].As
		if prefix == "" || slices.Contains(seen, module) {
			continue
		}
		seen = append(seen, module)
		services := moduleServiceUniques(ctx, workspace, module)
		if len(services) == 0 {
			continue
		}
		provisionedModules = append(provisionedModules, module)
		for _, unique := range services {
			overrides[unique] = map[string]string{moduleRegistrationSecretEnvironmentVariable: provisioned.byPrefix[prefix]}
		}
	}
	return overrides, provisionedModules
}

// moduleServiceUniques returns the module-qualified uniques of every service the
// named module declares, or nothing when the workspace does not reference it or
// cannot load it.
func moduleServiceUniques(ctx context.Context, workspace *resources.Workspace, module string) []string {
	for _, ref := range workspace.Modules {
		if ref.Name != module {
			continue
		}
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			return nil
		}
		uniques := make([]string, 0, len(mod.ServiceReferences))
		for _, svcRef := range mod.ServiceReferences {
			uniques = append(uniques, resources.ServiceUnique(mod.Name, svcRef.Name))
		}
		return uniques
	}
	return nil
}
