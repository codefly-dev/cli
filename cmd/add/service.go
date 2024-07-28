package add

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/communicate"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/services"

	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Add a service",

	Run: func(cmd *cobra.Command, args []string) {

		if interactive {
			cli.GetLogger().Oops("Interactive mode not implemented yet")
		}
		if len(args) != 1 {
			cli.GetLogger().Oops("You must provide a name for the module as the single argument")
		}
		if agentInput == "" {
			cli.GetLogger().Oops("You must provide an agent for the service, use --agent=<agent>, for example --agent=python, --agent=go, --agent=nextjs or more advanced agent. See TODO")
		}
		name := args[0]

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		errs := make(chan error, 1) // Buffered channel

		go func() {
			errs <- addService(ctx, name, agentInput)
		}()
	loop:
		for {
			select {
			case err := <-errs:
				cli.ExitOnError(err, "Got service add error: %v\n", err)
				// TODO: get rid when flow works
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				cli.Header(1, "TODO: Cleanup")
				break loop
			}
		}
		stopped := <-errs
		if stopped != nil {
			cli.Error("Got error while stopping: %v", stopped)
			return
		}
		cli.Header(1, "Service added successfully")
	},
}

func addService(ctx context.Context, name string, agentInput string) error {
	w := wool.Get(ctx).In("cmd.add.service")

	cli.SetWithDefault(withDefault)

	workspace := common.RequireWorkspace(ctx)

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
		mod = common.RequireModule(ctx)
	}

	if mod.ExistsService(ctx, svcWithMod.Name) && !override {
		cli.GetLogger().Oops("Service <%s> already exists", svcWithMod.Name)
	}

	w.Debug("input", wool.Field("agent", agentInput))

	agent, err := common.GetAgent(ctx, agentInput)
	if err != nil {
		return w.Wrapf(err, "cannot get agent")
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm adding a service <%s> in module <%s>?", svcWithMod.Name, mod.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		cli.Exit()
	}

	input := &actionsservice.AddService{
		Name:  svcWithMod.Name,
		Agent: agent.Proto(),
	}

	addDescription := models.Confirm(ctx, "Do you want to add a short description?", false)
	if addDescription {
		input.Description = models.Input("Description", "Make some magic 🪄")

	}

	output, err := services.Add(ctx, workspace, mod, input, communicate.NewPrompt())
	if err != nil {
		return w.Wrapf(err, "cannot add service")
	}

	// Show some information: Read me
	// TODO: Configuration output
	rendered, err := glamour.Render(output.ReadMe, "dark")
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
