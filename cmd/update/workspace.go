package update

import (
	"fmt"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// WorkspaceCmd represents the run command
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Update every workspace service to its latest compatible agent",

	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		workspace, err := common.LoadWorkspace(cmd.Context())
		if err != nil {
			return fmt.Errorf("cannot load workspace: %w", err)
		}
		return updateWorkspace(workspace)
	},
}

func updateWorkspace(workspace *resources.Workspace) error {
	ctx, done := common.NewContext()
	defer done()

	mods, err := workspace.LoadModules(ctx)
	if err != nil {
		return fmt.Errorf("cannot load modules: %w", err)
	}
	for _, mod := range mods {
		cli.Header(1, "Updating module <%s>", mod.Name)
		svcs, err := mod.LoadServices(ctx)
		if err != nil {
			return fmt.Errorf("cannot load services for module %s: %w", mod.Name, err)
		}
		for _, svc := range svcs {
			cli.Header(2, "Updating service <%s>", svc.Name)
			update, err := services.UpdateAgent(ctx, svc)
			if err != nil {
				return fmt.Errorf("cannot update service %s/%s: %w", mod.Name, svc.Name, err)
			}
			if update.AgentUpdate != nil {
				cli.Header(2, "Updating agent <%s> version: %s -> %s", update.AgentUpdate.Name, update.AgentUpdate.From, update.AgentUpdate.To)
			}
		}
	}
	return nil
}
