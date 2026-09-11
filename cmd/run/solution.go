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
// service reads by contract. Provisioning itself writes nothing to disk: the
// raw secret lives in the receiving processes' environment and does not outlive
// the run. The one way it reaches a file is --output-env, which exports one
// selected service's whole runtime environment to an owner-only path — and
// since a consumed module's service now carries a plaintext identity too, that
// export can name it as well as the backend (--output-env-service).

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
	// moduleRegistrationPrefixEnvironmentVariable and
	// moduleRegistrationSecretEnvironmentVariable carry ONE identity to the
	// consumed module's own service: the prefix it federates under and the
	// plaintext of the digest declared for it. A composed module presents that
	// secret to the gateway's /modules/_work-context to obtain the Work Context
	// its service principal calls the host's module-facing surface with
	// (module-saas-starter #568); without it the module can never authenticate
	// there. Singular on purpose — a module holds one identity, the solution
	// backend holds one per module it consumes; validateModuleIdentities
	// enforces that a declaration cannot ask for more.
	//
	// The secret is the SAME one the backend presents to register the facade:
	// the registrar verifies both exchanges against the single digest declared
	// for the prefix, so it cannot tell the backend registering "documents"
	// apart from the service principal of "documents". A backend therefore can
	// mint the Work Context of every module it consumes. Splitting registration
	// from identity needs a second digest the host reads, so it is a host-first
	// change (codefly-dev/cli#608), not one the CLI can make alone: emitting a
	// digest under a key no service declares would fail every module's exchange
	// closed.
	moduleRegistrationPrefixEnvironmentVariable = "CODEFLY__MODULE_REGISTRATION_PREFIX"
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
	secrets  string
	digests  string
	// byPrefix is each prefix's plaintext on its own, and owners the
	// module-qualified service uniques that federate under it — the consumed
	// module's own services, which present the same secret to obtain their
	// service principal's Work Context.
	byPrefix map[string]string
	owners   map[string][]string
}

// provisionModuleRegistrationSecrets mints a fresh secret for each distinct
// facade prefix among the consumed APIs. An entry with no facade entry-point is
// skipped: the runtime refuses to guess a prefix for it, so it will never
// register and a secret for it would authorize nothing.
func provisionModuleRegistrationSecrets(consumed []manifest.ConsumedAPI) (*moduleRegistrationSecrets, error) {
	prefixes := make([]string, 0, len(consumed))
	owners := make(map[string][]string, len(consumed))
	for i := range consumed {
		prefix := consumed[i].As
		if prefix == "" {
			continue
		}
		// Every entry names the service that serves the facade; two entries
		// sharing a prefix are one federated route, served by one module.
		if consumed[i].Module != "" && consumed[i].Service != "" {
			owner := resources.ServiceUnique(consumed[i].Module, consumed[i].Service)
			if !slices.Contains(owners[prefix], owner) {
				owners[prefix] = append(owners[prefix], owner)
			}
		}
		// Distinct prefixes only: the digests are declared once per prefix, so a
		// repeat contributes nothing to the registrar's side of the exchange.
		if slices.Contains(prefixes, prefix) {
			continue
		}
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	if err := validateModuleIdentities(prefixes, owners); err != nil {
		return nil, err
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
		secrets:  strings.Join(secrets, ","),
		digests:  strings.Join(digests, ","),
		byPrefix: byPrefix,
		owners:   owners,
	}, nil
}

// validateModuleIdentities refuses a projection the module-identity carrier
// cannot express. CODEFLY__MODULE_REGISTRATION_PREFIX/_SECRET name exactly one
// prefix and one secret, and the run folds prefix -> owners into a per-service
// override map, so both shapes below collapse silently at the assignment: one
// of them would drop an identity, the other would hand two modules the same
// one. Neither depends on the environment — they are wrong in every workspace,
// for any registrar — so they are rejected here, on the same footing as
// validateConsumedBindings and before the run starts anything, rather than
// discovered as a module that never authenticates.
func validateModuleIdentities(prefixes []string, owners map[string][]string) error {
	byOwner := make(map[string][]string, len(prefixes))
	order := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		// One facade, several services. Every owner would be handed the SAME
		// plaintext, so each could present it to the gateway exchange and obtain
		// the others' service-principal Work Context. manifest.Load rejects a
		// repeated `as`, but the run path decodes leniently and skips that gate
		// (loadSolutionManifestForRun), so this is the only thing that catches it.
		if len(owners[prefix]) > 1 {
			return fmt.Errorf("%s: api.consumes declares facade %q for more than one service (%s); one facade is one module's identity",
				manifest.FileName, prefix, strings.Join(owners[prefix], ", "))
		}
		for _, owner := range owners[prefix] {
			if len(byOwner[owner]) == 0 {
				order = append(order, owner)
			}
			byOwner[owner] = append(byOwner[owner], prefix)
		}
	}
	for _, owner := range order {
		// One service, several facades. Valid to core — it rejects a duplicated
		// (module, service, endpoint) triple, not a module serving two endpoints
		// under two prefixes — but the singular carrier holds only the last one
		// written, so the other facade could never obtain a Work Context.
		if claimed := byOwner[owner]; len(claimed) > 1 {
			return fmt.Errorf("%s: api.consumes gives %s more than one facade (%s); %s carries a single identity per service",
				manifest.FileName, owner, strings.Join(claimed, ", "), moduleRegistrationPrefixEnvironmentVariable)
		}
	}
	return nil
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

// workspaceResolvesService reports whether a module-qualified unique names a
// service this workspace can actually load.
//
// api.consumes names the facade's owner in manifest text, and a derived
// override is delivered by matching that text against a service in the run
// graph (Flow.overridesFor) — a key matching nothing is dropped without a word.
// So a composed module renamed in the composition, an entry naming the contract
// rather than the runnable service, or a module that resolves only remotely all
// produce an injection that reaches no process. Resolving first, the same way
// federationRegistrars does, is what lets the caller say so instead of
// announcing an identity it did not deliver.
func workspaceResolvesService(ctx context.Context, workspace *resources.Workspace, unique string) bool {
	target, err := resources.ParseServiceWithOptionalModule(unique)
	if err != nil || target.Module == "" {
		return false
	}
	_, err = workspace.LoadService(ctx, target)
	return err == nil
}
