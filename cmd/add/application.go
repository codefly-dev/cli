package add

import (
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsapplication "github.com/codefly-dev/core/actions/application"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Add an application",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		w := wool.Get(ctx).In("Add application")
		if interactive {
			logger.Oops("Interactive mode not implemented yet")
			return
		}
		if len(args) != 1 {
			logger.Oops("You must provide a name for the application as the single argument")
			return
		}
		addApplication(args[0])
	},
}

func addApplication(name string) {
	ctx := shared.NewContext()

	project := common.Project(ctx)

	if project.ExistsApplication(name) {
		cli.Error("Application <{{.}}> already exists", name)
		os.Exit(0)
	}

	confirm := models.Confirm(golor.Sprintf("Do you want to add an application in your project <{{.Name}}>?", project), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		os.Exit(0)
	}

	// Asks for Description
	addDescription := models.Confirm("Do you want to add a short description?", false)
	var desc string
	if addDescription {
		desc = models.Input("Description", "Make some magic 🪄")
	}

	action, err := actionsapplication.NewActionAddApplication(ctx, &actionsapplication.AddApplication{
		Name:        name,
		Description: desc,
		Project:     project.Name,
	})
	shared.UnexpectedExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action)
	if err != nil {
		shared.UnexpectedExitOnError(err, "cannot add application")
	}
	app, err := actions.As[configurations.Application](out)
	if err != nil {
		shared.UnexpectedExitOnError(err, "cannot add application")
	}
	cli.Header(2, "Application <{{.Name}}> added and is now active", app)
}

func init() {
	ApplicationCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
