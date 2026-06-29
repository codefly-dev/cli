package show

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// DependenciesCmd shows the SERVICE dependency graph and, for a given service, the
// topological start order to it. It reuses the SAME `architecture.ServiceDependencies`
// graph the run flow uses to order services — no parallel implementation — so what you
// see here is exactly what `codefly run` sequences on.
var DependenciesCmd = &cobra.Command{
	Use:   "dependencies [service]",
	Short: "Show the service dependency graph and the start order to a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()
		w := wool.Get(ctx).In("show.dependencies")

		workspace, _, service := common.LoadRequired(ctx, args)

		deps, err := architecture.NewServiceDependencies(ctx, workspace)
		if err != nil {
			w.Error("cannot build dependency graph", wool.ErrField(err))
			return
		}

		fmt.Println("Service dependency graph (X required by Y):")
		fmt.Println(deps.Print())

		if service == nil {
			return
		}
		id, err := service.Identity()
		if err != nil {
			w.Error("cannot get service identity", wool.ErrField(err))
			return
		}
		order, err := deps.OrderTo(ctx, id.Unique())
		if err != nil {
			w.Error("cannot compute start order", wool.ErrField(err))
			return
		}
		fmt.Printf("\nStart order to %s (dependencies first):\n", id.Unique())
		for i, s := range order {
			fmt.Printf("  %d. %s\n", i+1, s.Unique)
		}
	},
}
