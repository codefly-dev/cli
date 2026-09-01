package run

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/orchestration"
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

// resolveSolutionEntry finds the solution root: the single composed module that
// declares a service-entry, returning its "<module>/<service-entry>" unique.
// Composed modules that fail to resolve (e.g. a pinned coordinate with no local
// checkout yet) are not the local root, so their load errors are collected and
// only surfaced if no entry is found at all.
func resolveSolutionEntry(ctx context.Context, workspace *resources.Workspace) (string, error) {
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

func init() {
	// Solution-facing subset of the run flags, bound to the same package vars
	// runServiceCommand reads. The advanced/testing flags (cli-server,
	// temporary-ports, naming-scope, …) stay ServiceCmd-only.
	SolutionCmd.Flags().StringVar(&fixture, "fixture", "", "Fixture override (defaults to the selected Codefly environment)")
	SolutionCmd.Flags().StringVar(&environmentName, "env", orchestration.LocalEnvironmentName, "Workspace environment to run")
	SolutionCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY, e.g. MCP, CI, pipes)")
	SolutionCmd.Flags().StringVar(&profile, "profile", "", "Named workspace run profile")
	SolutionCmd.Flags().StringSliceVar(&excludeDependencies, "exclude-dependency", nil, "Exclude optional dependency services from the run (repeatable, e.g. infra/temporal)")
	SolutionCmd.Flags().StringSliceVar(&setOverrides, "set", nil, "Per-service runtime env override (repeatable), e.g. --set warden:CODEFLY__FIXTURE=dogfood")
	SolutionCmd.Flags().StringSliceVar(&silent, "silent", nil, "Silence services in CLI output")
}
