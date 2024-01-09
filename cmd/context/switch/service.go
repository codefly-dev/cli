package _switch

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	actions "github.com/codefly-dev/core/actions/actions"
	serviceactions "github.com/codefly-dev/core/actions/service"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Switch active service",
	Run: func(cmd *cobra.Command, args []string) {
		Service()
	},
}

func Service() {
	ctx, done := common.NewContext()
	defer done()

start:

	project := common.Project(ctx)
	app, err := project.LoadActiveApplication(ctx)

	cli.ExitOnError(err, "cannot get active application")
	all, err := app.LoadServices(ctx)
	cli.ExitOnError(err, "cannot get services")
	if len(all) == 0 {
		cli.Header(2, "No service found")
		return
	}
	apps := project.Applications
	if len(all) == 1 && len(apps) == 1 {
		cli.Header(2, "You have only one service and one application: <%s>. It is active by default.", all[0].Name)
		return
	}
	active, _ := app.LoadActiveService(ctx)

	var entries []*models.Entry
	if active != nil {
		entries = append(entries, &models.Entry{
			Identifier:  active.Name,
			Description: active.Description,
			Current:     true,
		})
	}
	for _, p := range all {
		if active != nil && p.Name == active.Name {
			continue
		}
		entries = append(entries, &models.Entry{
			Identifier:  p.Name,
			Description: p.Description,
		})
	}
	entries = append(entries, &models.Entry{
		Identifier: ">> In other application",
	})
	selected, err := models.Select("Make this service active", entries)
	cli.ExitOnError(err, "cannot select service")

	if selected.Identifier == ">> In other application" {
		switchApplication()
		goto start
	}

	if active != nil && selected.Identifier == active.Name {
		cli.Header(2, "All set!")
		return
	}

	if selected.Identifier == ">> In other application" {

	}

	action, err := serviceactions.NewActionSetServiceActive(ctx, &serviceactions.SetServiceActive{
		Name:        selected.Identifier,
		Application: app.Name,
		Project:     project.Name,
	})
	cli.ExitOnError(err, "cannot create action")

	_, err = actions.Run(ctx, action)
	cli.ExitOnError(err, "cannot set active service")

	cli.Header(2, "Active service is now: %s", selected.Identifier)

}
