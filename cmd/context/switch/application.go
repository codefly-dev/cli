package _switch

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/prompts"
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
	shared.UnexpectedExitOnError(err, "cannot get active application")
	all, err := project.LoadApplications(ctx)
	shared.UnexpectedExitOnError(err, "cannot get applications")
	if len(all) == 1 {
		cli.Header(2, "You have only one application")
		return
	}
	active := &prompts.Entry{
		Identifier:  application.Name,
		Description: application.Description,
		Current:     true,
	}
	entries := []*prompts.Entry{active}
	for _, p := range all {
		if p.Name == application.Name {
			continue
		}
		entries = append(entries, &prompts.Entry{
			Identifier:  p.Name,
			Description: p.Description,
		})
	}
	selected, err := prompts.Select("Make this application active", entries)
	shared.UnexpectedExitOnError(err, "cannot select application")

	if selected.Identifier == application.Name {
		cli.Header(2, "Active application is already: {{.Name}}", application)
		return
	}

	action, err := applicationactions.NewActionSetApplicationActive(ctx, &applicationactions.SetApplicationActive{
		Name:      selected.Identifier,
		InProject: project.Name,
	})
	shared.UnexpectedExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action)
	shared.UnexpectedExitOnError(err, "cannot set active application")

	now, err := actions.As[configurations.Application](out)
	shared.UnexpectedExitOnError(err, "cannot get active application")

	cli.Header(2, "Active application is now: {{.Name}}", now)

}
