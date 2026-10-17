package composition

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
)

// FromPins is the context a command producing an environment other than local
// loads the workspace under: ctx carrying PinnedView of the overlay, with each
// override it hides named (unit and path, never a value). A deploy must never
// depend on what one machine's codefly.local.yaml points at, and must never
// diverge from it silently either. The file is left as it is, so the next
// local run honours every override exactly as before.
func FromPins(ctx context.Context, workspace *resources.Workspace, environment string) (context.Context, error) {
	filter, ignored, err := PinnedView(ctx, workspace)
	if err != nil {
		return nil, err
	}
	for _, override := range ignored {
		cli.Info("ignoring local override for %s: %s (environment %s renders from pins)", override.Unit(), override.Path, environment)
	}
	if filter == nil {
		return ctx, nil
	}
	return resources.WithLocalOverlayFilter(ctx, filter), nil
}

// LocalOverride is one machine-local resolve directive that moves a composed
// module, or one service of it, off the source the workspace commits: a
// `resolve.<module>.path` or `worktree` the developer wrote, or any
// `resolve.<module>.services.<service>` entry. It names the unit and where the
// directive points — a path or a worktree coordinate, never a value.
type LocalOverride struct {
	Module  string
	Service string
	Path    string
}

// Unit is the override's target as a person names it: `<module>` or
// `<module>/<service>`.
func (override LocalOverride) Unit() string {
	if override.Service == "" {
		return override.Module
	}
	return override.Module + "/" + override.Service
}

// LocalOverrides lists the overrides the overlay on disk applies to the
// workspace's composed modules: what a local run honours, and what a render
// for any other environment ignores. The CLI's own entries — the path a pinned
// module was materialized to, a `pinned:`/`git:` selector of how to
// materialize it — are not overrides and are not listed.
func LocalOverrides(ctx context.Context, workspace *resources.Workspace) ([]LocalOverride, error) {
	state, err := loadMaterializationState(ctx, workspace)
	if err != nil || state.overlay == nil {
		return nil, err
	}
	return state.localOverrides(workspace.Modules), nil
}

func (state *materializationState) localOverrides(modules []*resources.ModuleReference) []LocalOverride {
	var overrides []LocalOverride
	for _, ref := range modules {
		directive := state.overlay.Resolve[ref.Name]
		if directive == nil {
			continue
		}
		if path := moduleOverridePath(ref, directive, state.receipts[ref.Name], state.roots); path != "" {
			overrides = append(overrides, LocalOverride{Module: ref.Name, Path: path})
		}
		for _, service := range sortedKeys(directive.Services) {
			overrides = append(overrides, LocalOverride{Module: ref.Name, Service: service, Path: serviceOverridePath(directive.Services[service])})
		}
	}
	return overrides
}

// moduleOverridePath is where a developer's directive moves the module itself,
// or "" when the entry is the CLI's own (pinnedManaged) or selects no location
// for the module.
func moduleOverridePath(ref *resources.ModuleReference, directive *resources.ModuleResolveDirective, receipt *ResolutionReceipt, roots moduleCacheRoots) string {
	switch {
	case directive.Worktree != "":
		return directive.Worktree
	case directive.Path == "":
		return ""
	case pinnedManaged(ref, directive, receipt.ResolvedPath(), roots.owned...):
		return ""
	}
	return directive.Path
}

func serviceOverridePath(directive *resources.ServiceResolveDirective) string {
	switch {
	case directive.Path != "":
		return directive.Path
	case directive.Worktree != "":
		return directive.Worktree
	}
	return "version " + directive.Version
}

// PinnedView is the view of the overlay a render for an environment other
// than local resolves with: every LocalOverride replaced by what committed
// configuration selects — a composed module at its pinned materialization, a
// service at the module's own copy — and every other entry as the file says.
// A pin a developer's override displaced is materialized here, into the same
// cache a run uses, under the same mode a run would choose for the module
// without that override; one that cannot be is an error, never a fallback to
// the override. Nothing is written to codefly.local.yaml or its receipts, so
// the overrides stay for the next local run. The filter is nil when the
// overlay holds no override, and the returned overrides are the ones ignored.
func PinnedView(ctx context.Context, workspace *resources.Workspace) (resources.LocalOverlayFilter, []LocalOverride, error) {
	state, err := loadMaterializationState(ctx, workspace)
	if err != nil || state.overlay == nil {
		return nil, nil, err
	}
	ignored := state.localOverrides(workspace.Modules)
	if len(ignored) == 0 {
		return nil, nil, nil
	}
	moved := map[string]bool{}
	for _, override := range ignored {
		if override.Service == "" {
			moved[override.Module] = true
		}
	}
	view := map[string]*resources.ModuleResolveDirective{}
	for _, ref := range workspace.Modules {
		directive := state.overlay.Resolve[ref.Name]
		if directive == nil {
			continue
		}
		entry := *directive
		entry.Services = nil
		if moved[ref.Name] {
			entry = resources.ModuleResolveDirective{}
			if ref.Source != "" && ref.PathOverride == nil {
				mode := ResolutionModeFor(nil, state.receipts[ref.Name], state.declared[ref.Name])
				resolved, err := materializeModule(ctx, workspace.Dir(), ref, state.roots.clone, mode)
				if err != nil {
					return nil, nil, fmt.Errorf("cannot resolve module %s from its pin in place of its local override: %w", ref.Name, err)
				}
				entry.Path = resolved.dir
			}
		}
		if selectsNothing(&entry) {
			view[ref.Name] = nil
		} else {
			view[ref.Name] = &entry
		}
	}
	filter := func(module string, directive *resources.ModuleResolveDirective) *resources.ModuleResolveDirective {
		if entry, ok := view[module]; ok {
			return entry
		}
		return directive
	}
	return filter, ignored, nil
}
