package add

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/services/services"

	"github.com/codefly-dev/core/actions/actions"
	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/spf13/cobra"
)

// ServiceDependencyCmd represents the run command
var ServiceDependencyCmd = &cobra.Command{
	Use:   "dependency",
	Short: "Add a service dependency",

	Run: func(cmd *cobra.Command, args []string) {
		if interactive {
			cli.GetLogger().Oops("Interactive mode not implemented yet")
		}
		addServiceDependency(args...)
	},
}

func addServiceDependency(args ...string) {
	ctx, done := common.NewContext()
	defer done()
	defer services.ClearAgents()

	project := common.RequireProject(ctx)

	// Argument overrides directory
	service, err := common.ParseServiceArgument(ctx, project, args)
	if err != nil {
		cli.ExitOnError(err, "Cannot parse service argument")
	}
	if service == nil {
		service = common.Service(ctx)
	}

	if service == nil {
		cli.Error("No service found: use argument or run inside a service folder or use workspace")
		return
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm adding a service dependency for <%s>?", service.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		cli.Exit()
	}

	app, err := project.LoadApplicationFromName(ctx, service.Application)
	cli.ExitOnError(err, "cannot get application from name")

	// First all services in the same application
	inAppServices := app.ServiceReferences

	// only 1 service means must be in another app
	if len(inAppServices) > 1 {
		var entries []*models.Entry
		for _, p := range inAppServices {
			if p.Name == service.Name {
				continue
			}
			entries = append(entries, &models.Entry{
				Identifier: p.Name,
			})
		}
		// Or another application
		otherApplication := ">> In another application"
		entries = append(entries, &models.Entry{
			Identifier: otherApplication,
		})
		selected, err := models.Select("Select the dependency or >> In another application", entries)
		cli.ExitOnError(err, "cannot select service dependency")

		if selected.Identifier != otherApplication {
			action, err := actionsservice.NewActionAddServiceDependency(ctx, &actionsservice.AddServiceDependency{
				Name:                  service.Name,
				ProjectPath:           project.Dir(),
				Application:           app.Name,
				DependencyApplication: app.Name,
				DependencyName:        selected.Identifier,
			})
			cli.ExitOnError(err, "cannot create action")
			_, err = actions.Run(ctx, action)
			cli.ExitOnError(err, "cannot add service dependency")
			cli.Header(2, "Service dependency added")
			return
		}
	}
	allApps := project.Applications
	if len(allApps) == 1 {
		cli.Error("No other application found")
		return
	}
	var entries []*models.Entry
	for _, p := range allApps {
		if p.Name == app.Name {
			continue
		}
		entries = append(entries, &models.Entry{
			Identifier: p.Name,
		})
	}
	selected, err := models.Select("Select the application for the dependent service", entries)
	cli.ExitOnError(err, "cannot select application")

	otherApp, err := project.LoadApplicationFromName(ctx, selected.Identifier)
	cli.ExitOnError(err, "cannot load application")
	entries = []*models.Entry{}
	for _, p := range otherApp.ServiceReferences {
		if p.Name == service.Name {
			continue
		}
		entries = append(entries, &models.Entry{
			Identifier: p.Name,
		})
	}
	selected, err = models.Select("Select the dependent service", entries)
	cli.ExitOnError(err, "cannot select service")

	action, err := actionsservice.NewActionAddServiceDependency(ctx, &actionsservice.AddServiceDependency{
		Name:                  service.Name,
		ProjectPath:           project.Dir(),
		Application:           app.Name,
		DependencyApplication: otherApp.Name,
		DependencyName:        selected.Identifier,
	})
	cli.ExitOnError(err, "cannot create action")
	_, err = actions.Run(ctx, action)
	cli.ExitOnError(err, "cannot add service dependency")
	cli.Header(2, "Service dependency added")

}
