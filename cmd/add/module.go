package add

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsmodule "github.com/codefly-dev/core/actions/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
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
var moduleWithDefault bool

func addModule(name string) {
	ctx, done := common.NewContext()
	defer done()

	w := wool.Get(ctx).In("cmd.add.module")

	// Non-interactive mode: skip all TUI prompts (for MCP, CI, scripts)
	cli.SetWithDefault(moduleWithDefault)

	workspace := common.RequireWorkspace(ctx)

	if workspace.ExistsModule(name) {
		cli.Error("Module <%s> already exists", name)
		os.Exit(0)
	}

	// In non-interactive mode (--yes), skip confirmation
	if !moduleWithDefault {
		confirm := models.Confirm(ctx, fmt.Sprintf("Add a module in your workspace <%s>?", workspace.Name), true)
		if !confirm {
			cli.Header(2, "Received loud and clear!")
			os.Exit(0)
		}
	}

	// Resolve module agent if specified
	var agent *resources.Agent
	if moduleAgentInput != "" {
		var err error
		agent, err = common.GetModuleAgent(ctx, moduleAgentInput)
		cli.ExitOnError(err, "cannot resolve module agent")
		cli.Header(2, "Using module template: %s", agent.Identifier())
	}

	input := &actionsmodule.AddModule{
		Name: name,
	}
	if agent != nil {
		input.Agent = agent.Proto()
	}

	action, err := actionsmodule.NewActionAddModule(ctx, input)
	cli.ExitOnError(err, "cannot create action")
	out, err := actions.Run(ctx, action, &actions.Space{Workspace: workspace})
	cli.ExitOnError(err, "cannot add module")

	mod, err := actions.As[resources.Module](out)
	cli.ExitOnError(err, "cannot add module")

	// If a module agent was specified, execute it to scaffold services and templates
	if agent != nil {
		binPath, err := agent.Path(ctx)
		if err != nil {
			w.Warn("cannot resolve module agent binary path", wool.ErrField(err))
		} else {
			w.Info("executing module agent", wool.Field("binary", binPath), wool.Field("dir", mod.Dir()))

			cmd := exec.CommandContext(ctx, binPath, mod.Dir(), name)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				cli.ExitOnError(err, "module agent failed")
			}
			cli.Header(2, "Module agent scaffolded services for <%s>", name)
		}
	}

	cli.Header(2, "Module <%s> added.", mod.Name)
}

func init() {
	ModuleCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
	ModuleCmd.Flags().StringVar(&moduleAgentInput, "agent", "", "Module template agent (e.g. user-management, rag)")
	ModuleCmd.Flags().BoolVar(&moduleWithDefault, "yes", false, "Skip confirmation prompts (non-interactive/MCP mode)")
}
