package add

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsmodule "github.com/codefly-dev/core/actions/module"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:   "module",
	Short: "Add a module",

	Run: func(cmd *cobra.Command, args []string) {

		if interactive {
			cli.Error("Interactive mode not implemented yet")
			cli.Exit()
		}
		if len(args) != 1 {
			cli.Error("You must provide a name for the module as the single argument")
			return
		}
		name := args[0]
		addModule(name)
	},
}

var moduleAgentInput string

func addModule(name string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.RequireWorkspace(ctx)

	if workspace.ExistsModule(name) {
		cli.Error("Module <%s> already exists", name)
		os.Exit(0)
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Add a module in your workspace <%s>?", workspace.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		os.Exit(0)
	}

	input := &actionsmodule.AddModule{
		Name: name,
	}

	// If a module agent/template was specified, resolve it
	if moduleAgentInput != "" {
		agent, err := common.GetModuleAgent(ctx, moduleAgentInput)
		cli.ExitOnError(err, "cannot resolve module agent")
		input.Agent = agent.Proto()
		cli.Header(2, "Using module template: %s", agent.Identifier())
	}

	var action actions.Action
	var err error

	action, err = actionsmodule.NewActionAddModule(ctx, input)
	cli.ExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action, &actions.Space{Workspace: workspace})
	if err != nil {
		cli.ExitOnError(err, "cannot add module")
	}
	app, err := actions.As[resources.Module](out)
	if err != nil {
		cli.ExitOnError(err, "cannot add module")
	}

	cli.Header(2, "Module <%s> added.", app.Name)
}

func init() {
	ModuleCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
	ModuleCmd.Flags().StringVar(&moduleAgentInput, "agent", "", "Module template agent (e.g. user-management, rag)")
}
