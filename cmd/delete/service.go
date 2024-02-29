package delete

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/configurations"
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
		name := args[0]
		deleteService(name)
	},
}

func deleteService(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.Workspace(ctx)
	project := common.Project(ctx)
	app := common.Application(ctx)

	// Parse service to see if we need to change organization
	parsed, err := configurations.ParseService(name)
	cli.ExitOnError(err, "Cannot parse service name")
	name = parsed.Name

	if parsed.Application != "" && parsed.Application != app.Name {
		app, err = project.LoadApplicationFromName(ctx, parsed.Application)
		cli.ExitOnError(err, "Cannot load application")
	}

	if !app.ExistsService(ctx, name) {
		cli.Error("Service <%s> does not exist in application <%s>", name, app.Name)
		return
	}
	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm deletion of service <%s> in application <%s> in project <%s>?", name, app.Name, project.Name), false)
	if confirm {
		err = app.DeleteService(ctx, name)
		cli.ExitOnError(err, "cannot delete service")
		err = project.DeleteServiceDependencies(ctx, &configurations.ServiceReference{Application: app.Name, Name: name})
		cli.ExitOnError(err, "cannot delete service dependencies")
		if workspace != nil && workspace.HasProject(project.Name) {
			err = workspace.DeleteService(ctx, project.Name, app.Name, name)
			cli.ExitOnError(err, "cannot delete service from workspace")
		}
		cli.Header(2, "Service <%s> deleted!", name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
}
