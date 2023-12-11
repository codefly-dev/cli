package add

import (
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/prompts"
	"github.com/codefly-dev/core/actions/actions"
	actionsapplication "github.com/codefly-dev/core/actions/application"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Add an application",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		logger := shared.GetLogger(ctx).With("Add application")
		if interactive {
			logger.Oops("Interactive mode not implemented yet")
		}
		if len(args) != 1 {
			logger.Oops("You must provide a name for the application as the single argument")
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

	confirm := prompts.Confirm(golor.Sprintf("Do you want to add an application in your project <{{.Name}}>?", project), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		os.Exit(0)
	}

	// Asks for Description
	addDescription := prompts.Confirm("Do you want to add a short description?", false)
	var desc string
	if addDescription {
		desc = prompts.Input("Description", "Make some magic 🪄")
	}

	action, err := actionsapplication.NewActionAddApplication(ctx, &actionsapplication.AddApplication{
		Name:        name,
		Description: desc,
		InProject:   project.Name,
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
