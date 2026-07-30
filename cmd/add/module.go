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
	Short: "Create a module to group related services and jobs",
	Args:  cobra.ExactArgs(1),

	RunE: func(cmd *cobra.Command, args []string) error {
		if interactive {
			return fmt.Errorf("interactive mode not implemented yet")
		}
		return addModule(args[0])
	},
}

var moduleAgentInput string
var moduleWithDefault bool

func addModule(name string) error {
	ctx, done := common.NewContext()
	defer done()

	w := wool.Get(ctx).In("cmd.add.module")

	// Non-interactive mode: skip all TUI prompts (for MCP, CI, scripts)
	cli.SetWithDefault(moduleWithDefault)

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	if workspace.ExistsModule(name) {
		return fmt.Errorf("module <%s> already exists", name)
	}

	// In non-interactive mode (--yes), skip confirmation
	if !moduleWithDefault {
		confirm := models.Confirm(ctx, fmt.Sprintf("Add a module in your workspace <%s>?", workspace.Name), true)
		if !confirm {
			cli.Header(2, "Received loud and clear!")
			return nil
		}
	}

	// Resolve module agent if specified
	var agent *resources.Agent
	if moduleAgentInput != "" {
		var err error
		agent, err = common.GetModuleAgent(ctx, moduleAgentInput)
		if err != nil {
			return fmt.Errorf("cannot resolve module agent: %w", err)
		}
		cli.Header(2, "Using module template: %s", agent.Identifier())
	}

	input := &actionsmodule.AddModule{
		Name: name,
	}
	if agent != nil {
		// Assign through input.Agent rather than a temporary: a `:=` here would
		// shadow the enclosing err.
		input.Agent, err = agent.Proto()
		if err != nil {
			return fmt.Errorf("cannot resolve agent %s: %w", agent.Identifier(), err)
		}
	}

	action, err := actionsmodule.NewActionAddModule(ctx, input)
	if err != nil {
		return fmt.Errorf("cannot create action: %w", err)
	}
	out, err := actions.Run(ctx, action, &actions.Space{Workspace: workspace})
	if err != nil {
		return fmt.Errorf("cannot add module: %w", err)
	}

	mod, err := actions.As[resources.Module](out)
	if err != nil {
		return fmt.Errorf("cannot read added module: %w", err)
	}

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
				return fmt.Errorf("module agent failed: %w", err)
			}
			cli.Header(2, "Module agent scaffolded services for <%s>", name)
		}
	}

	cli.Header(2, "Module <%s> added.", mod.Name)
	return nil
}

func init() {
	ModuleCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
	ModuleCmd.Flags().StringVar(&moduleAgentInput, "agent", "", "Module template agent (e.g. user-management, rag)")
	ModuleCmd.Flags().BoolVar(&moduleWithDefault, "yes", false, "Skip confirmation prompts (non-interactive/MCP mode)")
}
