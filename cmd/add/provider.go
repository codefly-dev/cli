package add

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ProviderCmd represents the run command
var ProviderCmd = &cobra.Command{
	Use:   "provider",
	Short: "Add an provider",

	Run: func(cmd *cobra.Command, args []string) {
		logger := shared.NewLogger("AddCmd.Provider")
		_, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
		defer stop()

		if len(args) == 0 || interactive {
			shared.Exit(`🥸Provide a name for your provider as the argument.`)
		}
		name := args[0]

		if plugin == "" {
			shared.Exit("need to specify a plugin: --plugin=<plugin-name>")
		}

		plugin, err := configurations.ParsePlugin(configurations.PluginService, plugin)
		if shared.IsUserWarning(err) {
			logger.Warn(err)
		} else {
			logger.Oops("cannot parse plugin: %s", plugin)
			os.Exit(1)
		}
		provider, err := configurations.NewProvider(name, plugin)
		shared.UnexpectedExitOnError(err, "cannot create provider")

		project := common.ProjectConfiguration(current)
		fmt.Println("Add provider to project", project.Name)

		err = project.AddProvider(provider)
		shared.ExitOnError(err, "cannot add provider")
	},
}

func init() {
	ProviderCmd.Flags().BoolVarP(&current, "current", "c", false, "use current project")
	ProviderCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
	ProviderCmd.Flags().StringVar(&plugin, "plugin", "", "Service plugin to get started")
}
