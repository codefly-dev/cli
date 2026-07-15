package delete

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Delete a service",
	Args:  cobra.ExactArgs(1),

	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}
		return deleteService(workspace, module, service)
	},
}

func deleteService(workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	ctx, done := common.NewContext()
	defer done()

	mod, err := workspace.LoadModuleFromName(ctx, module.Name)
	if err != nil {
		return fmt.Errorf("cannot load module: %w", err)
	}

	if !mod.ExistsService(ctx, service.Name) {
		return fmt.Errorf("service <%s> does not exist in module <%s>", service.Name, mod.Name)
	}
	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm deletion of service <%s> in module <%s> in workspace <%s>?", service.Name, mod.Name, workspace.Name), false)
	if confirm {
		err = mod.DeleteService(ctx, service.Name)
		if err != nil {
			return fmt.Errorf("cannot delete service: %w", err)
		}
		err = workspace.DeleteServiceDependencies(ctx, &resources.ServiceReference{Module: mod.Name, Name: service.Name})
		if err != nil {
			return fmt.Errorf("cannot delete service dependencies: %w", err)
		}
		cli.Header(2, "Service <%s> deleted!", service.Name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
	return nil
}
