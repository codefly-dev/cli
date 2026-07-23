package add

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/communicate"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/tui"

	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Create an agent-backed service in a module",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if interactive {
			return fmt.Errorf("interactive mode not implemented yet")
		}
		if agentInput == "" {
			return fmt.Errorf("service agent is required; use --agent=<agent>")
		}

		cli.Init()
		defer services.ClearAgents()

		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		if err := addService(ctx, args[0], agentInput); err != nil {
			return fmt.Errorf("cannot add service: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Service added successfully")
		return nil
	},
}

func addService(ctx context.Context, name string, agentInput string) error {
	w := wool.Get(ctx).In("cmd.add.service")

	cli.SetWithDefault(withDefault)

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot load workspace")
	}

	svcWithMod, err := resources.ParseServiceWithOptionalModule(name)
	if err != nil {
		return w.Wrapf(err, "cannot parse service name")
	}

	var mod *resources.Module
	if svcWithMod.Module != "" {
		mod, err = workspace.LoadModuleFromName(ctx, svcWithMod.Module)
		if err != nil {
			return w.Wrapf(err, "cannot load module")
		}
	} else {
		mod, err = common.LoadModule(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot load active module")
		}
	}

	if mod.ExistsService(ctx, svcWithMod.Name) && !override {
		return w.NewError("service <%s> already exists", svcWithMod.Name)
	}

	w.Debug("input", wool.Field("agent", agentInput))

	agent, err := common.GetAgent(ctx, agentInput)
	if err != nil {
		return w.Wrapf(err, "cannot get agent")
	}

	confirm, err := models.ConfirmE(ctx, fmt.Sprintf("Confirm adding a service <%s> in module <%s>?", svcWithMod.Name, mod.Name), true)
	if err != nil {
		return w.Wrapf(err, "cannot confirm service creation")
	}
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		return nil
	}

	input := &actionsservice.AddService{
		Name:  svcWithMod.Name,
		Agent: agent.Proto(),
	}

	addDescription, err := models.ConfirmE(ctx, "Do you want to add a short description?", false)
	if err != nil {
		return w.Wrapf(err, "cannot confirm service description")
	}
	if addDescription {
		input.Description, err = models.Input("Description", "Make some magic 🪄")
		if err != nil {
			return w.Wrapf(err, "cannot read service description")
		}

	}

	output, err := services.Add(ctx, workspace, mod, input, communicate.NewPrompt())
	if err != nil {
		return w.Wrapf(err, "cannot add service")
	}

	// Show some information: Read me
	// TODO: Configuration output
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
	agentInput  string
	withDefault bool
)

func init() {
	ServiceCmd.Flags().StringVar(&agentInput, "agent", "", "Instance agent to get started")
	ServiceCmd.Flags().BoolVar(&override, "override", false, "Override existing service")
	ServiceCmd.Flags().BoolVar(&withDefault, "default", false, "Use default options")

}
