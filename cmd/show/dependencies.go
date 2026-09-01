package show

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// DependenciesCmd shows the SERVICE dependency graph and, for a given service, the
// topological start order to it. It reuses the SAME `architecture.ServiceDependencies`
// graph the run flow uses to order services — no parallel implementation — so what you
// see here is exactly what `codefly run` sequences on.
var DependenciesCmd = &cobra.Command{
	Use:   "dependencies [service]",
	Short: "Show a service's dependency graph and startup order",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("cannot load workspace: %w", err)
		}
		var service *resources.Service
		if len(args) > 0 {
			_, _, service, err = common.LoadRequiredE(ctx, args)
			if err != nil {
				return err
			}
		}

		deps, err := architecture.NewServiceDependencies(ctx, workspace)
		if err != nil {
			return fmt.Errorf("cannot build dependency graph: %w", err)
		}

		fmt.Println("Service dependency graph (X required by Y):")
		fmt.Println(deps.Print())

		printModuleResolution(ctx, workspace)

		if service == nil {
			return nil
		}
		id, err := service.Identity()
		if err != nil {
			return fmt.Errorf("cannot get service identity: %w", err)
		}
		order, err := deps.OrderTo(ctx, id.Unique())
		if err != nil {
			return fmt.Errorf("cannot compute start order: %w", err)
		}
		fmt.Printf("\nStart order to %s (dependencies first):\n", id.Unique())
		for i, s := range order {
			fmt.Printf("  %d. %s\n", i+1, s.Unique)
		}
		return nil
	},
}

// printModuleResolution reports where each composed module resolves — a local
// path, a matched git worktree, or a pinned artifact coordinate — so a
// composition root's identity/overlay split is visible in one place. Each module
// resolves independently: a directive that fails to resolve (e.g. a worktree
// with no local checkout) is reported inline rather than aborting the report.
func printModuleResolution(ctx context.Context, workspace *resources.Workspace) {
	if len(workspace.Modules) == 0 {
		return
	}
	fmt.Println("\nModule resolution (identity → location):")
	for _, ref := range workspace.Modules {
		res, err := workspace.ResolveModule(ctx, ref)
		if err != nil {
			fmt.Printf("  %s: unresolved (%v)\n", ref.Name, err)
			continue
		}
		switch res.Kind {
		case resources.ResolutionWorktree:
			fmt.Printf("  %s: worktree %s@%s → %s\n", res.Module, res.Source, res.Ref, res.Dir)
		case resources.ResolutionPinned:
			fmt.Printf("  %s: pinned %s@%s\n", res.Module, res.Source, res.Version)
		default:
			fmt.Printf("  %s: path %s\n", res.Module, res.Dir)
		}
	}
}
