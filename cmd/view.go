package cmd

import (
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ViewCmd represents the build command
var ViewCmd = &cobra.Command{
	Use:   "view",
	Short: "View your applications",
	Run: func(cmd *cobra.Command, args []string) {
		m := observability.NewManager()
		workspace, err := m.Load()
		shared.ExitOnError(err, "cannot load management")
		server, err := web.NewServer(web.ServerData{Workspace: workspace})
		shared.ExitOnError(err, "cannot create applications server")
		err = server.Start(cmd.Context())
		shared.ExitOnError(err, "cannot start applications server")
	},
}
