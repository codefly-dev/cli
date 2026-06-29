package show

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

var namingScope string

// NetworkCmd shows every service's endpoints and the DETERMINISTIC native address each
// binds to — computed with network.NativeFor, the SAME port hash the run flow's
// GenerateNetworkMappings uses. So this is the network configuration a run produces,
// visible WITHOUT starting anything, plus the dependency endpoints each service consumes.
var NetworkCmd = &cobra.Command{
	Use:   "network",
	Short: "Show each service's endpoints, the address it binds to, and its dependency endpoints",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()
		w := wool.Get(ctx).In("show.network")

		workspace, _, _ := common.LoadRequired(ctx, args)

		deps, err := architecture.NewServiceDependencies(ctx, workspace)
		if err != nil {
			w.Error("cannot build dependency graph", wool.ErrField(err))
			return
		}

		fmt.Printf("Network configuration for workspace %q (naming-scope=%q):\n\n", workspace.Name, namingScope)
		for _, s := range deps.Services() {
			svc, err := deps.ServiceFromUnique(s.Unique)
			if err != nil {
				continue
			}
			id, err := svc.Identity()
			if err != nil {
				continue
			}
			fmt.Printf("• %s\n", s.Unique)

			endpoints, err := svc.LoadEndpoints(ctx)
			if err != nil {
				fmt.Printf("    cannot load endpoints: %v\n", err)
			} else if len(endpoints) == 0 {
				fmt.Println("    (no endpoints exposed)")
			}
			for _, ep := range endpoints {
				addr := "(external — DNS/public-resolved at runtime)"
				if ep.Visibility != resources.VisibilityExternal {
					if inst := network.NativeFor(ctx, workspace.Name, id.Module, id.Name, namingScope, ep); inst != nil {
						addr = inst.Address
					}
				}
				vis := ep.Visibility
				if vis == "" {
					vis = "private"
				}
				fmt.Printf("    %-8s api=%-6s visibility=%-8s → %s\n", ep.Name, ep.Api, vis, addr)
			}

			for _, dep := range svc.ServiceDependencies {
				wanted := "(all endpoints)"
				if len(dep.Endpoints) > 0 {
					wanted = ""
					for i, e := range dep.Endpoints {
						if i > 0 {
							wanted += ", "
						}
						wanted += e.Name
					}
				}
				fmt.Printf("    ↳ needs %s [%s]\n", dep.Unique(), wanted)
			}
		}
	},
}

func init() {
	NetworkCmd.Flags().StringVar(&namingScope, "naming-scope", "", "naming scope used to derive deterministic ports (matches `run --naming-scope`)")
}
