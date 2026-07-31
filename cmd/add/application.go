package add

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/applications"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/tui"

	actionapplication "github.com/codefly-dev/core/actions/application"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the add application command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Create an agent-backed application in a module",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if interactive {
			return fmt.Errorf("interactive mode not implemented yet")
		}
		if appAgentInput == "" {
			return fmt.Errorf("application agent is required; use --agent=<agent>")
		}

		cli.Init()
		defer services.ClearAgents()

		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		if err := addApplication(ctx, args[0], appAgentInput); err != nil {
			return fmt.Errorf("cannot add application: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Application added successfully")
		return nil
	},
}

func addApplication(ctx context.Context, name string, agentInput string) error {
	w := wool.Get(ctx).In("cmd.add.application")

	cli.SetWithDefault(appWithDefault)

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot load workspace")
	}

	appWithMod, err := resources.ParseServiceWithOptionalModule(name)
	if err != nil {
		return w.Wrapf(err, "cannot parse application name")
	}

	var mod *resources.Module
	if appWithMod.Module != "" {
		mod, err = workspace.LoadModuleFromName(ctx, appWithMod.Module)
		if err != nil {
			return w.Wrapf(err, "cannot load module")
		}
	} else {
		mod, err = common.LoadModule(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load active module")
		}
	}

	if mod.ExistsApplication(ctx, appWithMod.Name) && !appOverride {
		return w.NewError("application <%s> already exists", appWithMod.Name)
	}

	w.Debug("input", wool.Field("agent", agentInput))

	agent, err := common.GetApplicationAgent(ctx, agentInput)
	if err != nil {
		return w.Wrapf(err, "cannot get agent")
	}

	confirm, err := models.ConfirmE(ctx, fmt.Sprintf("Confirm adding an application <%s> in module <%s>?", appWithMod.Name, mod.Name), true)
	if err != nil {
		return w.Wrapf(err, "cannot confirm application creation")
	}
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		return nil
	}

	agentProto, err := agent.Proto()
	if err != nil {
		return w.Wrapf(err, "cannot resolve agent %s", agent.Identifier())
	}
	input := &actionapplication.AddApplication{
		Name:  appWithMod.Name,
		Agent: agentProto,
	}

	addDescription, err := models.ConfirmE(ctx, "Do you want to add a short description?", false)
	if err != nil {
		return w.Wrapf(err, "cannot confirm application description")
	}
	if addDescription {
		input.Description, err = models.Input("Description", "Build amazing things")
		if err != nil {
			return w.Wrapf(err, "cannot read application description")
		}
	}

	output, err := applications.Add(ctx, workspace, mod, input)
	if err != nil {
		return w.Wrapf(err, "cannot add application")
	}

	// Show some information: Read me
	rendered, err := tui.RenderMarkdown(output.ReadMe, "dark")
	if err != nil {
		return w.Wrapf(err, "cannot render info README")
	}
	// Paginate if long
	if len(strings.Split(rendered, "\n")) > 50 {
		cli.Paginate(rendered)
	} else {
		fmt.Println(rendered)
	}

	return nil
}

var (
	appAgentInput  string
	appWithDefault bool
	appOverride    bool
)

func init() {
	ApplicationCmd.Flags().StringVar(&appAgentInput, "agent", "", "Agent to build/run the application")
	ApplicationCmd.Flags().BoolVar(&appOverride, "override", false, "Override existing application")
	ApplicationCmd.Flags().BoolVar(&appWithDefault, "default", false, "Use default options")
}
