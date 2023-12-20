package add

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/services"

	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Add an service",

	Run: func(cmd *cobra.Command, args []string) {

		if interactive {
			common.CLI().Oops("Interactive mode not implemented yet")
		}
		if len(args) != 1 {
			common.CLI().Oops("You must provide a name for the application as the single argument")
		}
		if agent == "" {
			common.CLI().Oops("You must provide an agent for the service, use --agent=<agent>, for example --agent=python, --agent=go, --agent=nextjs or more advanced agent. See TODO")
		}
		name := args[0]
		addService(name, agent)
	},
}

func addService(name string, agentInput string) {
	ctx, done := common.NewContext()
	defer done()
	defer agents.ClearAgents()

	w := wool.Get(ctx).In("cmd.add.service")

	project := common.Project(ctx)
	app := common.Application(ctx)

	if app.ExistsService(name) && !override {
		common.CLI().Oops("Service <{{.}}> already exists", name)
	}

	w.Debug("input", wool.Field("agent", agent))
	agent, err := configurations.ParseAgent(ctx, configurations.ServiceAgent, agentInput)
	cli.ExitOnError(err, "Cannot parse agent")

	confirm := models.Confirm(golor.Sprintf("Confirm adding a service in your application <{{.Name}}>?", app), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		cli.Exit()
	}

	input := &actionsservice.AddService{
		Name:        name,
		Project:     project.Name,
		Application: app.Name,
		Agent:       agent.Proto(),
		Override:    override,
	}

	addDescription := models.Confirm("Do you want to add a short description?", false)
	if addDescription {
		input.Description = models.Input("Description", "Make some magic 🪄")

	}

	err = services.Add(ctx, input)
	cli.ExitOnError(err, "Cannot add service")

}

var (
	namespace string
)

func init() {
	ServiceCmd.Flags().StringVar(&agent, "agent", "", "Instance agent to get started")
	ServiceCmd.Flags().StringVar(&namespace, "namespace", "", "Namespace for the service, default to application name")
	ServiceCmd.Flags().BoolVar(&override, "override", false, "Override existing service")

}
