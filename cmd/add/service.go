package add

import (
	"os"

	"github.com/charmbracelet/glamour"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/prompts"
	"github.com/codefly-dev/core/actions/actions"
	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Add an service",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		logger := shared.GetLogger(ctx).With("Add application")
		if interactive {
			logger.Oops("Interactive mode not implemented yet")
		}
		if len(args) != 1 {
			logger.Oops("You must provide a name for the application as the single argument")
		}
		if agent == "" {
			logger.Oops("You must provide an agent for the service, use --agent=<agent>, for example --agent=python, --agent=go, --agent=nextjs or more advanced agent. See TODO")
		}
		addService(args[0], agent)
	},
}

func addService(name string, agentInput string) {
	defer agents.ClearAgents()
	ctx := shared.NewContext()
	logger := shared.GetLogger(ctx).With("addService")

	project := common.Project(ctx)
	app := common.Application(ctx)

	if app.ExistsService(name) && !override {
		cli.Error("Service <{{.}}> already exists", name)
		os.Exit(1)
	}

	if !withDefault {
		confirm := prompts.Confirm(golor.Sprintf("Confirm adding a service in your application <{{.Name}}>?", app), true)
		if !confirm {
			cli.Header(2, "Received loud and clear!")
			os.Exit(0)
		}
	}

	agent, err := configurations.ParseAgent(ctx, configurations.ServiceAgent, agentInput)
	shared.ExitOnError(err, "cannot parse agent input: %s", agentInput)

	logger.Debugf("agent input %s", agentInput)

	logger.TODO("SHOULD DOWNLOAD NOW")
	if agent.Version == "latest" {
		err = agents.PinToLatestRelease(configurations.AgentFromProto(agent))
		shared.ExitOnError(err, "cant get latest version")
	}

	var desc string
	if !withDefault {
		addDescription := prompts.Confirm("Do you want to add a short description?", false)
		if addDescription {
			desc = prompts.Input("Description", "Make some magic 🪄")
		}

	}

	action, err := actionsservice.NewActionAddService(ctx, &actionsservice.AddService{
		Name:          name,
		Description:   desc,
		InProject:     project.Name,
		InApplication: app.Name,
		Agent:         agent,
		Override:      override,
	})
	shared.ExitOnError(err, "cannot create action")

	out, err := actions.Run(ctx, action)
	shared.ExitOnError(err, "cannot add service")
	service, err := actions.As[configurations.Service](out)
	shared.ExitOnError(err, "cannot add service")

	cli.Header(2, "Service <{{.Name}}> added and is now active", service)

	instance, err := services.Load(ctx, service)
	shared.ExitOnError(err, "cannot load service instance")

	if instance.Factory != nil {
		cli.Header(2, "Service <{{.Name}}> has a factory", service)
		init, err := instance.Init(ctx)
		shared.ExitOnError(err, "cannot create service instance")
		// README
		rendered, err := glamour.Render(init.Readme, "dark")
		shared.ExitOnError(err, "cannot render readme")
		cli.Paginate(rendered)

		_, err = instance.Create(ctx)
		shared.ExitOnError(err, "cannot create service instance")
		logger.TODO("Communicate")
	}

}

var (
	namespace string
)

func init() {
	ServiceCmd.Flags().StringVar(&agent, "agent", "", "Instance agent to get started")
	ServiceCmd.Flags().StringVar(&namespace, "namespace", "", "Namespace for the service, default to application name")
	ServiceCmd.Flags().BoolVar(&override, "override", false, "Override existing service")
	ServiceCmd.Flags().BoolVar(&withDefault, "with-default", false, "Default on withDefault")

}
