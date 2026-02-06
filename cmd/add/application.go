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
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/applications"

	actionapplication "github.com/codefly-dev/core/actions/application"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/wool"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the add application command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Add an application",

	Run: func(cmd *cobra.Command, args []string) {

		if interactive {
			cli.GetLogger().Oops("Interactive mode not implemented yet")
		}
		if len(args) != 1 {
			cli.GetLogger().Oops("You must provide a name for the application as the single argument")
		}
		if appAgentInput == "" {
			cli.GetLogger().Oops("You must provide an agent for the application, use --agent=<agent>, for example --agent=go-cli or --agent=tauri")
		}
		name := args[0]

		cli.Init()

		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		errs := make(chan error, 1)

		go func() {
			errs <- addApplication(ctx, name, appAgentInput)
		}()
	loop:
		for {
			select {
			case err := <-errs:
				cli.ExitOnError(err, "Got application add error: %v\n", err)
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
		cli.Header(1, "Application added successfully")
	},
}

func addApplication(ctx context.Context, name string, agentInput string) error {
	w := wool.Get(ctx).In("cmd.add.application")

	cli.SetWithDefault(appWithDefault)

	workspace := common.RequireWorkspace(ctx)

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
		mod = common.RequireModule(ctx)
	}

	if mod.ExistsApplication(ctx, appWithMod.Name) && !appOverride {
		cli.GetLogger().Oops("Application <%s> already exists", appWithMod.Name)
	}

	w.Debug("input", wool.Field("agent", agentInput))

	agent, err := common.GetApplicationAgent(ctx, agentInput)
	if err != nil {
		return w.Wrapf(err, "cannot get agent")
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm adding an application <%s> in module <%s>?", appWithMod.Name, mod.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		cli.Exit()
	}

	input := &actionapplication.AddApplication{
		Name:  appWithMod.Name,
		Agent: agent.Proto(),
	}

	addDescription := models.Confirm(ctx, "Do you want to add a short description?", false)
	if addDescription {
		input.Description = models.Input("Description", "Build amazing things")
	}

	output, err := applications.Add(ctx, workspace, mod, input)
	if err != nil {
		return w.Wrapf(err, "cannot add application")
	}

	// Show some information: Read me
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
	appAgentInput  string
	appWithDefault bool
	appOverride    bool
)

func init() {
	ApplicationCmd.Flags().StringVar(&appAgentInput, "agent", "", "Agent to build/run the application")
	ApplicationCmd.Flags().BoolVar(&appOverride, "override", false, "Override existing application")
	ApplicationCmd.Flags().BoolVar(&appWithDefault, "default", false, "Use default options")
}
