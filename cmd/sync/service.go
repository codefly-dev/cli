package sync

import (
	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Sync a service",

	Run: func(cmd *cobra.Command, args []string) {
		app := configurations.MustCurrentApplication()

		var config *configurations.Service
		var err error

		if len(args) > 0 {
			ref := &configurations.ServiceReference{Name: "TODO"}
			config, err = configurations.FindServiceFromReference(ref)
		} else {
			config, err = configurations.FindUp[configurations.Service](".")
		}
		shared.ExitOnError(err, "cannot load service configuration")

		refresher, err := services.NewSyncer(app)
		shared.ExitOnError(err, "cannot create service refresher")

		err = refresher.Sync(config)
		shared.ExitOnError(err, "cannot sync service")
	},
}
