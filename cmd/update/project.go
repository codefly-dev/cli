package update

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// WorkspaceCmd represents the run command
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Update a workspace",

	Run: func(cmd *cobra.Command, args []string) {
		workspace := common.Workspace(context.Background())
		updateWorkspace(workspace)
	},
}

func updateWorkspace(workspace *resources.Workspace) {
	ctx, done := common.NewContext()
	defer done()

	mods, err := workspace.LoadModules(ctx)
	cli.ExitOnError(err, "cannot load modules")
	for _, mod := range mods {
		cli.Header(1, "Updating module <%s>", mod.Name)
		svcs, err := mod.LoadServices(ctx)
		cli.ExitOnError(err, "cannot load services")
		for _, svc := range svcs {
			cli.Header(2, "Updating service <%s>", svc.Name)
			update, err := services.UpdateAgent(ctx, svc)
			cli.ExitOnError(err, "cannot update service")
			if update.AgentUpdate != nil {
				cli.Header(2, "Updating agent <%s> version: %s -> %s", update.AgentUpdate.Name, update.AgentUpdate.From, update.AgentUpdate.To)
			}
		}
	}
}
