package run

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
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
		if err = composition.MaterializePinnedModules(ctx, workspace); err != nil {
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
		entry, err := ResolveSolutionEntry(ctx, workspace)
		done()
		if err != nil {
			return err
		}
		// Delegate to the run-service path with the resolved entry. It reloads
		// the workspace and boots the full dependency graph — reusing every run
		// flag default seeded by ServiceCmd's init, plus the solution-facing
		// flags registered below (which bind the same package vars). The pins are
		// declared resolved: materialization above already ran unconditionally,
		// so letting the delegate run it again would re-attempt any pull that
		// warned there — a second round trip and a duplicate warning, for a
		// request nothing has changed since.
		pinsAlreadyResolved = true
		defer func() { pinsAlreadyResolved = false }()
		return runServiceCommand(cmd, []string{entry})
	},
}

// ResolveSolutionEntry finds the solution root and returns its
// "<module>/<service-entry>" unique. The root is the module declaring a
// service-entry that no other entry-declaring module depends on: a composed
// host (e.g. the saas host) declares an entry of its own, but the solution's
// entry depends on it — through its services' dependencies or its manifest's
// api.consumes — which makes the host a dependency, not a competing root. That
// holds whether the solution is the workspace's own module or one composed by
// source and version beside the host, so a product composition that composes
// both runs the solution without naming it.
//
// When more than one entry is left — nothing depends on either — the
// workspace's own module (`path: .`, or the one named like the workspace) is
// the root, being what the workspace is for. Composed modules that fail to
// resolve (e.g. a pinned coordinate with no local checkout yet) are not the
// root, so their load errors are collected and only surfaced if no entry is
// found.
func ResolveSolutionEntry(ctx context.Context, workspace *resources.Workspace) (string, error) {
	var entries []*resources.Module
	var loadErrs []error
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %w", ref.Name, err))
			continue
		}
		if mod.ServiceEntry != "" {
			entries = append(entries, mod)
		}
	}
	if len(entries) == 0 {
		if len(loadErrs) > 0 {
			return "", fmt.Errorf("no solution root in workspace <%s>: no resolvable module declares a service-entry (some modules failed to resolve: %w)", workspace.Name, errors.Join(loadErrs...))
		}
		return "", fmt.Errorf("no solution root in workspace <%s>: no module declares a service-entry", workspace.Name)
	}
	roots := solutionrun.SolutionRoots(ctx, workspace, entries)
	if len(roots) == 0 {
		// Every entry depends on another: a cycle leaves nothing to prefer on
		// dependency grounds, so the tie-breaker below decides among them all.
		roots = entries
	}
	if len(roots) == 1 {
		return entryUnique(roots[0]), nil
	}
	if self := solutionrun.RootRef(workspace); self != nil {
		for _, root := range roots {
			if root.Name == self.Name {
				return entryUnique(root), nil
			}
		}
	}
	uniques := make([]string, 0, len(roots))
	for _, root := range roots {
		uniques = append(uniques, entryUnique(root))
	}
	return "", fmt.Errorf("ambiguous solution root in workspace <%s>: multiple modules declare a service-entry and none depends on another (%s); run `codefly run service <module/service>` explicitly", workspace.Name, strings.Join(uniques, ", "))
}

// entryUnique is the "<module>/<service-entry>" unique of a module's entry.
func entryUnique(mod *resources.Module) string {
	return resources.ServiceUnique(mod.Name, mod.ServiceEntry)
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
	SolutionCmd.Flags().StringVar(&namingScope, "naming-scope", "", NamingScopeUsage)
	SolutionCmd.Flags().BoolVar(&temporaryPorts, "temporary-ports", false, TemporaryPortsUsage)
}
