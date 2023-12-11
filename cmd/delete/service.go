package delete

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/display"
	"github.com/codefly-dev/cli/pkg/cli/prompts"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Delete a service",

	Run: func(cmd *cobra.Command, args []string) {
		if len(args) != 1 {
			shared.Exit("You must provide a name for the service as the single argument")
		}
		name := args[0]
		deleteService(name)
	},
}

func deleteService(name string) {
	ctx := shared.NewContext()
	project := common.Project(ctx)
	app := common.Application(ctx)
	if !app.ExistsService(name) {
		cli.Error("Service <{{.Other.Service}}> does not exist in application <{{.Application.Name}}>", display.New().WithProject(project).WithApplication(app).With("Service", name))
		return
	}
	confirm := prompts.Confirm(golor.Sprintf("Confirm deletion of service <{{.Other.Service}}> in application <{{.Application.Name}}> in project <{{.Project.Name}}>?",
		display.New().WithProject(project).WithApplication(app).With("Service", name)), false)
	if confirm {
		err := app.DeleteService(ctx, name)
		shared.UnexpectedExitOnError(err, "cannot delete service")
		cli.Header(2, "Service <{{.}}> deleted!", name)
	} else {
		cli.Header(2, "Abort! Heard loud and clear.")
	}
}
