package cmd

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

// ServerCmd represents the build command
var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Server for codefly",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		defer services.ClearAgents()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("cannot load workspace: %w", err)
		}
		server, err := web.NewServer(web.ServerData{Workspace: workspace})
		if err != nil {
			return fmt.Errorf("cannot create web server: %w", err)
		}
		if err := server.Start(ctx); err != nil {
			return fmt.Errorf("server failed: %w", err)
		}
		return nil
	},
}
