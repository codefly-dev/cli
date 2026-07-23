package initialize

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsworkspace "github.com/codefly-dev/core/actions/workspace"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// WorkspaceCmd represents the run command
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Create a Codefly workspace in a new directory",
	Args:  cobra.ExactArgs(1),

	RunE: func(cmd *cobra.Command, args []string) error {
		if interactive {
			return fmt.Errorf("interactive mode not implemented yet")
		}
		cli.SetWithDefault(withDefault)
		return newWorkspace(args[0])
	},
}

var (
	layout      string
	withDefault bool
)

func newWorkspace(name string) error {
	ctx, done := common.NewContext()
	defer done()
	ctx, stop := common.SignalContext(ctx)
	defer stop()

	cur, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot get current directory: %w", err)
	}

	confirm, err := models.ConfirmE(ctx, fmt.Sprintf("codefly will create a workspace <%s> in the current folder. Proceed?", name), true)
	if err != nil {
		return fmt.Errorf("cannot confirm workspace creation: %w", err)
	}
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		return nil
	}

	selectedLayout := layout
	if selectedLayout == "" {
		entries := []*models.Entry{
			{Identifier: resources.LayoutKindFlat, Description: "Flat layout (no modules), good for simple projects", Current: true},
			{Identifier: resources.LayoutKindModules, Description: "Modules layout, good for complex projects with multiple components"},
		}
		choice, choiceErr := models.Choice(ctx, `Choose the style of the workspace:

For very simple projects, pick a flat layout where all services are in the root module:

workspace/
├── 📂 configurations
|   ├── 📂 ${dev}
│   └── 📂 ${production}
└── 📂 services
│   ├── 📂 ${frontend}
│   ├── 📂 ${backend}
│   └── 📂 ${database}

For more complex projects, pick a modules layout to group your services:

workspace/
├── 📂 configurations
|   ├── 📂 ${dev}
│   └── 📂 ${prod}
└── 📂 modules
│   └── 📂 ${management}
|       └── 📂services
|           ├── 📂 ${backend}
|           ├── 📂 ${cache}
|           └── 📂 ${database}
│   └── 📂 ${external}
|           ├── 📂 ${frontend}
|           └── 📂 ${api}
`, entries)
		if choiceErr != nil {
			return fmt.Errorf("cannot choose workspace layout: %w", choiceErr)
		}
		if choice == nil {
			return fmt.Errorf("workspace layout selection returned no choice")
		}
		selectedLayout = choice.Identifier
	}

	var action actions.Action
	action, err = actionsworkspace.NewActionNewWorkspace(ctx, &actionsworkspace.NewWorkspace{
		Name:   name,
		Layout: selectedLayout,
		Path:   cur,
	})
	if err != nil {
		return fmt.Errorf("cannot create workspace action: %w", err)
	}

	out, err := actions.Run(ctx, action, nil)
	if err != nil {
		return fmt.Errorf("cannot create workspace: %w", err)
	}

	workspace, err := actions.As[resources.Workspace](out)
	if err != nil {
		return fmt.Errorf("cannot read created workspace: %w", err)
	}

	cli.Header(2, "Workspace <%s> created in current directory", workspace.Name)
	return ctx.Err()
}

func init() {
	WorkspaceCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
	WorkspaceCmd.PersistentFlags().StringVar(&layout, "layout", "", "workspace layout: flat or modules")
	WorkspaceCmd.PersistentFlags().BoolVar(&withDefault, "default", false, "use default values for all prompts")
}
