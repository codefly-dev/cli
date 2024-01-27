package add

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/services/actions"

	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/configurations"
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
			cli.GetLogger().Oops("You must provide a name for the application as the single argument")
		}
		if agentInput == "" {
			cli.GetLogger().Oops("You must provide an agent for the service, use --agent=<agent>, for example --agent=python, --agent=go, --agent=nextjs or more advanced agent. See TODO")
		}
		name := args[0]

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
	defer agents.ClearAgents()

	w := wool.Get(ctx).In("cmd.add.service")

	workspace := common.Workspace(ctx)
	project := common.Project(ctx)
	app := common.Application(ctx)

	// Parse service to see if we need to change organization
	parsed, err := configurations.ParseService(name)
	if err != nil {
		return w.Wrapf(err, "cannot parse service name")
	}

	if parsed.Application != "" {
		name = parsed.Name
		if parsed.Application != app.Name {
			app, err = project.LoadApplicationFromName(ctx, parsed.Application)
			if err != nil {
				return w.Wrapf(err, "cannot load application")
			}
		}
	}

	if app.ExistsService(ctx, name) && !override {
		cli.GetLogger().Oops("Service <%s> already exists", name)
	}

	w.Debug("input", wool.Field("agent", agentInput))
	agent, err := configurations.ParseAgent(ctx, configurations.ServiceAgent, agentInput)
	if err != nil {
		return w.Wrapf(err, "cannot parse agent")
	}

	// Pin to latest if needed
	if agent.Version == "latest" {
		err = manager.PinToLatestRelease(ctx, agent)
		if err != nil {
			return w.Wrapf(err, "cannot pin to latest release")
		}
	}

	// Download the agent if required
	if !manager.Downloaded(agent) {
		err = manager.Download(ctx, agent)
		if err != nil {
			return w.Wrapf(err, "cannot download agent")
		}
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm adding a service <%s> in application <%s>?", name, app.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		cli.Exit()
	}

	input := &actionsservice.AddService{
		Name:        name,
		Project:     project.Name,
		Workspace:   workspace.Name,
		Application: app.Name,
		Agent:       agent.Proto(),
		Override:    override,
	}

	addDescription := models.Confirm(ctx, "Do you want to add a short description?", false)
	if addDescription {
		input.Description = models.Input("Description", "Make some magic 🪄")

	}

	err = actions.Add(ctx, input)
	if err != nil {
		return w.Wrapf(err, "cannot add service")
	}
	return nil

}

var (
	namespace  string
	agentInput string
)

func init() {
	ServiceCmd.Flags().StringVar(&agentInput, "agent", "", "Instance agent to get started")
	ServiceCmd.Flags().StringVar(&namespace, "namespace", "", "Namespace for the service, default to application name")
	ServiceCmd.Flags().BoolVar(&override, "override", false, "Override existing service")

}
