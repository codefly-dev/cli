package cmd

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ServerCmd represents the build command
var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Server for codefly",
	Run: func(cmd *cobra.Command, args []string) {

		ctx := shared.NewContext()
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		errs := make(chan error, 1) // Buffered channel

		go func() {
			w, err := web.NewServer(web.ServerData{})
			shared.ExitOnError(err, "cannot create applications server")
			errs <- w.Start(ctx)
		}()

		for {
			select {
			case err := <-errs:
				if err != nil {
					fmt.Printf("Got applications run error: %v\n", err)
				}
			case <-ctx.Done():
				fmt.Println("Got context.Cancel: Exiting...")
			}
		}

	},
}
