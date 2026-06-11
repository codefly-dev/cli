package cmd

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/spf13/cobra"
)

// ServerCmd represents the build command
var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Server for codefly",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		errs := make(chan error, 1) // Buffered channel

		workspace := common.Workspace(ctx)
		if workspace == nil {
			cli.Error("No workspace found")
			cli.ExitError()
		}
		go func() {
			w, err := web.NewServer(web.ServerData{Workspace: workspace})
			cli.ExitOnError(err, "cannot create web server")
			errs <- w.Start(ctx)
		}()

	loop:
		for {
			select {
			case err := <-errs:
				if err != nil {
					fmt.Printf("Got modules run error: %v\n", err)
				}
				break loop
			case <-ctx.Done():
				fmt.Println("Got context.Cancel: Exiting...")
				break loop
			}
		}

	},
}
