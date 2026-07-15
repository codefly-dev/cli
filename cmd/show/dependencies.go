package show

import (
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
	Short: "Show the service dependency graph and the start order to a service",
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
