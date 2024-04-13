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
	"github.com/codefly-dev/cli/pkg/services/services"

	actionsservice "github.com/codefly-dev/core/actions/service"
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

	//workspace := common.Workspace(ctx)
	project := common.RequireProject(ctx)

	app := common.Application(ctx)

	// Parse service to see if we need to change organization
	parsed, err := configurations.ParseService(name)
	if err != nil {
		return w.Wrapf(err, "cannot parse service name")
	}

	if parsed.Application != "" {
		name = parsed.Name
		// Choice of creating an application if not present
		created := false
		if app == nil {
			addApplication(name)
			created = true
			project, err = configurations.ReloadProject(ctx, project)
			cli.ExitOnError(err, "cannot reload project")

		}
		if created || parsed.Application != app.Name {
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

	agent, err := common.GetAgent(ctx, agentInput)
	if err != nil {
		return w.Wrapf(err, "cannot get agent")
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm adding a service <%s> in application <%s>?", name, app.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		cli.Exit()
	}

	input := &actionsservice.AddService{
		Name:            name,
		ApplicationPath: app.Dir(),
		Agent:           agent.Proto(),
		Override:        override,
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
	agentInput  string
	withDefault bool
)

func init() {
	ServiceCmd.Flags().StringVar(&agentInput, "agent", "", "Instance agent to get started")
	ServiceCmd.Flags().BoolVar(&override, "override", false, "Override existing service")
	ServiceCmd.Flags().BoolVar(&withDefault, "default", false, "Use default options")

}
