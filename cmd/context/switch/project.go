package _switch

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	actions "github.com/codefly-dev/core/actions/actions"
	projectactions "github.com/codefly-dev/core/actions/project"
	"github.com/codefly-dev/core/configurations"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Switch active project",
	Run: func(cmd *cobra.Command, args []string) {
		switchProject()
	},
}

func switchProject() {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.Workspace(ctx)
	project := common.Project(ctx)
	all, err := workspace.LoadProjects(ctx)
	cli.ExitOnError(err, "cannot get projects")
	if len(all) == 1 {
		cli.Header(2, "You have only one project")
		return
	}
	active := &models.Entry{
		Identifier:  project.Name,
		Description: project.Description,
		Current:     true,
	}
	entries := []*models.Entry{active}
	for _, p := range all {
		if p.Name == project.Name {
			continue
		}
		entries = append(entries, &models.Entry{
			Identifier:  p.Name,
			Description: p.Description,
		})
	}
	selected, err := models.Select("Make this project active", entries)
	cli.ExitOnError(err, "cannot select project")

	action, err := projectactions.NewActionSetProjectActive(ctx, &projectactions.SetProjectActive{
		Name:      selected.Identifier,
		Workspace: workspace.Name,
	})
	cli.ExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action)
	cli.ExitOnError(err, "cannot set active project")

	project, err = actions.As[configurations.Project](out)
	cli.ExitOnError(err, "cannot get active project")

	cli.Header(2, "Active project is now: <%s>", project.Name)

	activeApplication := common.Application(ctx)
	cli.Header(2, "Active application is now: <%s>", activeApplication.Name)

}
