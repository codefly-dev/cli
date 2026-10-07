// Package solutionrun derives what a composed solution needs at run time: the
// api.consumes projection its backend federates the consumed modules' routes
// from.
//
// It lives under pkg so one derivation serves every run path. pkg/control
// depends only downward and never on cmd, so a derivation owned by the run
// command could not reach the lifecycles it drives.
package solutionrun

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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
// no projection, and every route it consumed stayed unrouted.
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

// ModuleSolution reports whether a composed module is a solution instance, by
// returning the solution manifest it ships — or nil when it ships none. It is
// the same read every run derivation here performs, exported so a render can
// ask the question of a module it has already resolved rather than walking the
// composition a second time to answer it.
func ModuleSolution(module *resources.Module) (*manifest.Manifest, error) {
	if module == nil {
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

// validateConsumedBindings rejects a partially bound api.consumes entry and a
// facade prefix claimed twice. The lenient decode above skips manifest.Validate,
// so this is the only gate a run passes through, and both rules are ones the
// projection itself depends on.
//
// A partial bind — ConsumedAPIs() drops only entries with an empty module — would
// project into a CODEFLY__ENDPOINT key built from empty segments, which no
// runtime can resolve.
//
// A repeated prefix is unroutable: the solution runtime derives one /v1/<as>
// route per prefix, so two entries claiming one prefix leave it unable to say
// which module the route reaches. manifest.Validate rejects a duplicated `as`
// for sync and package; a run must not be the path that lets the same typo boot
// a solution those verbs refuse.
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
		if first, duplicate := seenAs[declaration.As]; duplicate {
			return fmt.Errorf("%s: api.consumes entries %q and %q both claim the facade prefix %q; one prefix is one route", manifest.FileName, first, declaration.ID, declaration.As)
		}
		seenAs[declaration.As] = declaration.ID
	}
	return nil
}

// RunInputs is what a run derives for a solution: per-service process
// overrides, and what the derivation wants an operator told. Returned together
// (never assigned to a global from inside) so a run that derives nothing clears
// both, and a second in-process invocation cannot inherit the previous run's
// values.
type RunInputs struct {
	Overrides map[string]map[string]string
	// Notes is what the derivation wants an operator told, in order.
	Notes []Note
}

// Note is one line of narration, returned rather than printed. This package is
// called both by the run command, which owns a terminal, and by the control
// plane, which runs inside a process whose stdout is a JSON-RPC stream — the
// MCP server serves on it — where a narration line corrupts the protocol. Only
// a caller that knows it owns a terminal can decide to render these.
type Note struct {
	// Warning marks a line an operator has to act on rather than a statement
	// of what was supplied.
	Warning bool
	Message string
}

// DerivedRunInputs resolves the solution injection for the service being run,
// when it is the service-entry of a module shipping a solution manifest: the
// CODEFLY__API_CONSUMES projection its backend federates the consumed modules'
// routes from. Every run path derives it here so `run service <entry>`, `run
// solution` and the control plane inject identically, and it reports what it
// sent through Notes: the value rides a Start override, which each service
// agent chooses to honor, so an operator debugging dead federation must be able
// to see that the CLI supplied it before suspecting the manifest.
//
// Nothing here depends on the module being the workspace's own: a solution
// composed by source and version derives the same input from the manifest in
// its cache checkout.
func DerivedRunInputs(module *resources.Module, service *resources.Service, serviceName string) (RunInputs, error) {
	solutionManifest, err := entryManifest(module, service)
	if err != nil || solutionManifest == nil {
		return RunInputs{}, err
	}
	consumed := solutionManifest.ConsumedAPIs()
	if len(consumed) == 0 {
		return RunInputs{}, nil
	}
	return RunInputs{
		Overrides: map[string]map[string]string{
			serviceName: {manifest.APIConsumesEnvironmentVariable: solutionManifest.ConsumedAPIsEnvValue()},
		},
		Notes: []Note{{Message: fmt.Sprintf("injecting %s into %s: %s",
			manifest.APIConsumesEnvironmentVariable, serviceName, strings.Join(consumedIDs(consumed), ", "))}},
	}, nil
}

// consumedIDs lists the ids of the consumed APIs, in declaration order, for the
// narration of what a projection carries.
func consumedIDs(consumed []manifest.ConsumedAPI) []string {
	ids := make([]string, 0, len(consumed))
	for i := range consumed {
		ids = append(ids, consumed[i].ID)
	}
	return ids
}

// Merge layers the inputs derived for one root of a run over those derived for
// another. Overrides merge key by key, later winning, exactly as mergeOverrides
// does; notes keep their order across roots.
func Merge(base, layer RunInputs) RunInputs {
	return RunInputs{
		Overrides: mergeOverrides(base.Overrides, layer.Overrides),
		Notes:     append(append([]Note{}, base.Notes...), layer.Notes...),
	}
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
