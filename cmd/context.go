package cmd

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/context"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ContextCmd represents the Context command
var ContextCmd = &cobra.Command{
	Use:   "context",
	Short: "codefly context",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		// Determine what we are working on
		project := common.Project(ctx)
		cli.Header(2, "⭐️ Active project <{{.Name}}>", project)
		if app := project.ActiveApplication(); app == nil {
			cli.Header(2, "⚡️ No active application")
			return
		} else {
			cli.Header(2, "⚡️ Active application <{{.}}>", *app)

			app, err := project.LoadActiveApplication(ctx)
			cli.ExitOnError(err, "cannot load active application")
			if service := app.ActiveService(ctx); service == nil {
				cli.Header(2, "🔥 No active service")
				return
			} else {
				cli.Header(2, "🔥 Active service <{{.}}>", *service)
			}

		}
	},
}

func init() {
	ContextCmd.AddCommand(context.SwitchCmd)
}
