package add

import (
	"os"

	promptseervices "github.com/codefly-dev/cli/pkg/cli/prompts/services"
	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/agents"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli/prompts/create"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Add an service",

	Run: func(cmd *cobra.Command, args []string) {
		defer agents.ClearAgents()

		if len(args) == 0 || interactive {
			shared.Exit(`🥸Provide a name for your service as the argument.`)
		}

		project := common.ProjectConfiguration(current)
		app := common.ApplicationConfiguration(current)

		logger := shared.NewLogger("create.service<%s>", app.Name)

		name := args[0]
		err := configurations.ValidateServiceName(name)
		shared.ExitOnError(err, "invalid service name: %s", name)

		if agent == "" {
			shared.Exit("need to specify a agent: --agent=<agent-name>")
		}

		agent, err := configurations.ParseAgent(configurations.AgentService, agent)
		if err != nil {
			if shared.IsUserWarning(err) {
				logger.Warn(err)
			} else {
				logger.Oops("cannot parse agent: %s", agent)
				os.Exit(1)
			}
		}
		logger.Debugf("agent %s", agent)
		if agent.Version == "latest" {
			err = agents.PinToLatestRelease(agent)
			shared.ExitOnError(err, "cant get latest version")
		}

		confirm, err := promptseervices.Add(name, agent, app)
		shared.ExitOnError(err, "cannot prompt for service")
		if !confirm {
			shared.Exit("Received loud and clear!")
		}

		input := &services.CreationInput{
			Name:              name,
			Namespace:         namespace,
			Agent:             agent,
			RequiredBy:        requiredBy,
			DependsOn:         dependsOn,
			WithClientDecider: create.NewClientBuilder(),
			Application:       app,
		}
		logger.Debugf("creating service <%s> from agent <%s>", input.Name, agent.Name())
		err = input.SetNamespace(namespace)
		shared.ExitOnError(err, "cannot set namespace: %s", namespace)

		err = services.Add(input, configurations.WithProject(project), configurations.WithApplication(app))
		shared.ExitOnError(err, "cannot add service")
		err = app.Save()
		shared.ExitOnError(err, "cannot save application configuration")

	},
}

var (
	namespace  string
	requiredBy []string
	dependsOn  []string
)

func init() {
	ServiceCmd.Flags().BoolVarP(&current, "current", "c", false, "Use the current application")
	ServiceCmd.Flags().StringVar(&agent, "agent", "", "Instance agent to get started")
	ServiceCmd.Flags().StringVar(&namespace, "namespace", "default", "Namespace where to deploy the service")
	ServiceCmd.Flags().StringSliceVar(&requiredBy, "required-by", nil, "Other services requiring this service")
	ServiceCmd.Flags().StringSliceVar(&dependsOn, "depends-on", nil, "Other services this service depends on")
}
