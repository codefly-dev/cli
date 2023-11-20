package add

import (
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli/prompts/create"
	"github.com/codefly-dev/cli/pkg/plugins"
	"github.com/codefly-dev/cli/pkg/plugins/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Add an service",

	Run: func(cmd *cobra.Command, args []string) {
		defer plugins.ClearPlugins()

		if len(args) == 0 || interactive {
			shared.Exit(`🥸Provide a name for your service as the argument.`)
		}

		project := common.ProjectConfiguration(current)
		config := common.ApplicationConfiguration(current)

		logger := shared.NewLogger("create.service<%s>", config.Name)

		name := args[0]
		err := configurations.ValidateServiceName(name)
		shared.ExitOnError(err, "invalid service name: %s", name)

		if plugin == "" {
			shared.Exit("need to specify a plugin: --plugin=<plugin-name>")
		}

		plugin, err := configurations.ParsePlugin(configurations.PluginService, plugin)
		if err != nil {
			if shared.IsUserWarning(err) {
				logger.Warn(err)
			} else {
				logger.Oops("cannot parse plugin: %s", plugin)
				os.Exit(1)
			}
		}
		logger.DebugMe("plugin %s", plugin)

		// Give a new overview of the application
		//manager := management.NewManager()
		//app, err := manager.LoadApplication(config, project)
		//
		//shared.UnexpectedExitOnError(err, "cannot load application")

		input := &services.CreationInput{
			Name:              name,
			Namespace:         namespace,
			Plugin:            plugin,
			RequiredBy:        requiredBy,
			DependsOn:         dependsOn,
			WithClientDecider: create.NewClientBuilder(),
			Application:       config,
		}
		logger.Debugf("creating service <%s> from plugin <%s>", input.Name, plugin.Name())
		err = input.SetNamespace(namespace)
		shared.ExitOnError(err, "cannot set namespace: %s", namespace)

		err = services.Add(input, configurations.WithProject(project), configurations.WithApplication(config))
		shared.ExitOnError(err, "cannot add service")
		//
		//		isFirst := ""
		//		if len(configurations.MustCurrentApplication().Services) == 1 {
		//			isFirst = "#(bold)[first] "
		//		}
		//		golor.Println(`#(blue)[Successfully created your {{.IsFirst}}service for your applications <{{.Name}}>!]
		//#(italic,cyan)[You are ready to go, run this and start building cool things!]
		//#(italic,white)[codefly run applications]`, map[string]string{
		//			"Name": configurations.MustCurrentApplication().Name,
		//			"IsFirst":     isFirst,
		//		})
	},
}

var (
	namespace  string
	requiredBy []string
	dependsOn  []string
)

func init() {
	ServiceCmd.Flags().BoolVarP(&current, "current", "c", false, "Use the current application")
	ServiceCmd.Flags().StringVar(&plugin, "plugin", "", "Instance plugin to get started")
	ServiceCmd.Flags().StringVar(&namespace, "namespace", "default", "Namespace where to deploy the service")
	ServiceCmd.Flags().StringSliceVar(&requiredBy, "required-by", nil, "Other services requiring this service")
	ServiceCmd.Flags().StringSliceVar(&dependsOn, "depends-on", nil, "Other services this service depends on")
}
