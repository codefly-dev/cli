package cmd

import (
	"os/exec"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// OpenCmd represents the build command
var OpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open your applications",
	Run: func(cmd *cobra.Command, args []string) {
		var app *configurations.Application
		var err error
		if len(args) > 0 {
			name := args[0]
			app, err = configurations.LoadApplicationFromName(name)
			shared.ExitOnError(err, "cannot load application <%s>", name)
		} else {
			app = common.ApplicationConfiguration(true)
		}
		shared.ExitOnError(err, "cannot get current application")
		c := exec.Command("goland", app.Dir())
		err = c.Run()
		shared.ExitOnError(err, "cannot open application")
	},
}
