package delete

import (
	"fmt"
	"os"
	"os/signal"

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

	Run: func(cmd *cobra.Command, args []string) {
		if len(args) != 1 {
			cli.Error("You must provide a name for the service as the single argument")
			cli.Exit()
		}
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, service := common.LoadRequired(ctx, args)
		deleteService(workspace, module, service)
	},
}

func deleteService(workspace *resources.Workspace, module *resources.Module, service *resources.Service) {
	ctx, done := common.NewContext()
	defer done()

	mod, err := workspace.LoadModuleFromName(ctx, module.Name)
	cli.ExitOnError(err, "cannot load module")

	if !mod.ExistsService(ctx, service.Name) {
		cli.Error("Service <%s> does not exist in module <%s>", service.Name, mod.Name)
		return
	}
	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm deletion of service <%s> in module <%s> in workspace <%s>?", service.Name, mod.Name, workspace.Name), false)
	if confirm {
		err = mod.DeleteService(ctx, service.Name)
		cli.ExitOnError(err, "cannot delete service")
		err = workspace.DeleteServiceDependencies(ctx, &resources.ServiceReference{Module: mod.Name, Name: service.Name})
		cli.ExitOnError(err, "cannot delete service dependencies")
		cli.Header(2, "Service <%s> deleted!", service.Name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
}
