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

		active, err := common.LoadActiveContext(ctx)
		cli.ExitOnError(err, "cannot load active context")

		cli.Header(2, "⭐️ Active project <%s>", active.Project.Name)
		if active.Application == nil {
			cli.Header(2, "⚡️ No active application")
			return
		} else {
			cli.Header(2, "⚡️ Active application <%s>", active.Application.Name)
			if active.Service == nil {
				cli.Header(2, "🔥 No active service")
				return
			} else {
				cli.Header(2, "🔥 Active service <%s>", active.Service.Name)
				cli.ExitOnError(err, "cannot load active active")
				if len(active.Service.ServiceDependencies) > 0 {
					cli.Header(2, "  🚀 Service dependencies")
					for _, dep := range active.Service.ServiceDependencies {
						cli.Header(2, "   👉 <%s>", dep.Unique())
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
