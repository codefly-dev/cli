package _switch

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	actions "github.com/codefly-dev/core/actions/actions"
	applicationactions "github.com/codefly-dev/core/actions/application"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Switch active application",
	Run: func(cmd *cobra.Command, args []string) {
		switchApplication()
	},
}

func switchApplication() {
	// Workspace
	ctx := shared.NewContext()
	project := common.Project(ctx)
	application, err := project.LoadActiveApplication(ctx)
	cli.ExitOnError(err, "cannot get active application")
	all, err := project.LoadApplications(ctx)
	cli.ExitOnError(err, "cannot get applications")
	if len(all) == 1 {
		cli.Header(2, "You have only one application")
		return
	}
	active := &models.Entry{
		Identifier:  application.Name,
		Description: application.Description,
		Current:     true,
	}
	entries := []*models.Entry{active}
	for _, p := range all {
		if p.Name == application.Name {
			continue
		}
		entries = append(entries, &models.Entry{
			Identifier:  p.Name,
			Description: p.Description,
		})
	}
	selected, err := models.Select("Make this application active", entries)
	cli.ExitOnError(err, "cannot select application")

	if selected.Identifier == application.Name {
		cli.Header(2, "Active application is already: {{.Name}}", application)
		return
	}

	action, err := applicationactions.NewActionSetApplicationActive(ctx, &applicationactions.SetApplicationActive{
		Name:    selected.Identifier,
		Project: project.Name,
	})
	cli.ExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action)
	cli.ExitOnError(err, "cannot set active application")

	now, err := actions.As[configurations.Application](out)
	cli.ExitOnError(err, "cannot get active application")

	cli.Header(2, "Active application is now: {{.Name}}", now)

}
