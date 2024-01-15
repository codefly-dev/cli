package cmd

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/context"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// ContextCmd represents the Context command
var ContextCmd = &cobra.Command{
	Use:   "context",
	Short: "codefly context",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		// Determine what we are working on
		project := common.Project(ctx)
		cli.Header(2, "⭐️ Active project <%s>", project.Name)
		if app := project.ActiveApplication(ctx); app == nil {
			cli.Header(2, "⚡️ No active application")
			return
		} else {
			cli.Header(2, "⚡️ Active application <%s>", *app)

			app, err := project.LoadActiveApplication(ctx)
			cli.ExitOnError(err, "cannot load active application")
			if active := app.ActiveService(ctx); active == nil {
				cli.Header(2, "🔥 No active active")
				return
			} else {
				cli.Header(2, "🔥 Active service <%s>", *active)
				service, err := app.LoadActiveService(ctx)
				cli.ExitOnError(err, "cannot load active active")
				if len(service.Dependencies) > 0 {
					cli.Header(2, "🚀 Dependencies")
					for _, dep := range service.Dependencies {
						cli.Header(2, "🚀 Dependency <%s>", dep.Unique())
					}
				}
			}

		}
	},
}

func init() {
	ContextCmd.AddCommand(context.SwitchCmd)
	ContextCmd.AddCommand(context.ViewCmd)
}
