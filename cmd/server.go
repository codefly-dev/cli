package cmd

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

// openDashboardOnServer opens the dashboard in the browser once the server is up (used by `codefly server --open`).
var openDashboardOnServer bool

// ServerCmd represents the build command
var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Serve the local dashboard for the current workspace (attaches to a running codefly when one is up)",
	Long: `Serve the local dashboard for the current workspace.

If a "codefly run service --cli-server" is already serving this workspace's
dashboard, attach to it: print its URL and exit without starting a second
server.

Otherwise, start an inventory-only dashboard: the Services, Logs and Config
tabs show declared inventory but no live runtime state, since no run is
attached.`,
	Args: cobra.NoArgs,
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

		restPort := network.CLIRestPort(workspace.Name)
		restHostPort := fmt.Sprintf("127.0.0.1:%d", restPort)
		url := fmt.Sprintf("http://%s", restHostPort)

		if common.DashboardAttached(ctx, workspace.Name) {
			cli.Info("Dashboard already served by a running codefly for workspace %s: %s", workspace.Name, url)
			if openDashboardOnServer {
				if openErr := common.OpenBrowser(url); openErr != nil {
					cli.Warning("cannot open browser: %v", openErr)
				}
			}
			return nil
		}

		server, err := web.NewServer(web.ServerData{Workspace: workspace})
		if err != nil {
			return fmt.Errorf("cannot create web server: %w", err)
		}

		cli.Warning("No run is attached: the Services, Logs and Config tabs show declared inventory only. Start a run with `codefly run service <name> --cli-server` to see live state.")
		if url != "" {
			common.AnnounceDashboardWhenReady(ctx, url, openDashboardOnServer)
		}

		if err := server.Start(ctx); err != nil {
			return fmt.Errorf("server failed: %w", err)
		}
		return nil
	},
}

func init() {
	ServerCmd.Flags().BoolVar(&openDashboardOnServer, "open", false, "Open the dashboard in the default browser")
}
