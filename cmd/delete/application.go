package delete

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/display"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Delete an application",

	Run: func(cmd *cobra.Command, args []string) {
		if len(args) != 1 {
			shared.Exit("You must provide a name for the application as the single argument")
		}
		name := args[0]

		deleteApplication(name)
	},
}

func deleteApplication(name string) {
	ctx := shared.NewContext()
	project := common.Project(ctx)
	if !project.ExistsApplication(name) {
		cli.Error("Application <{{.Application}}> does not exist in project<{{.Project.Name}}>",
			display.New().WithProject(project))
		return
	}
	confirm := models.Confirm(golor.Sprintf("Do you want to delete the application <{{.Project.Name}} in project <{{.other.application}}>?",
		display.New().WithProject(project).With("application", name)), false)
	if confirm {
	}
	err := project.DeleteApplication(ctx, name)
	shared.UnexpectedExitOnError(err, "cannot delete application")
}
