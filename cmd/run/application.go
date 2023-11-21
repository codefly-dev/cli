package run

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/management"
	"github.com/codefly-dev/cli/pkg/plugins"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Run an application",
	Run: func(cmd *cobra.Command, args []string) {

		project := common.ProjectConfiguration(current)

		var app *configurations.Application
		var err error
		// Optional application argument
		if len(args) > 0 {
			name := args[0]
			app, err = configurations.LoadApplicationFromName(name, configurations.WithProject(project))
			shared.ExitOnError(err, "cannot load application <%s>", name)
		} else {
			app = common.ApplicationConfiguration(current)
		}
		run(project, app)
	},
}

func run(project *configurations.Project, config *configurations.Application) {
	logger := shared.NewLogger("run.ApplicationCmd")

	// Create a context that is cancelled on os.Interrupt or os.Kill
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer stop()

	var waitOnServer sync.WaitGroup
	errs := make(chan error, 1) // Buffered channel

	configurations.SetMode(configurations.ModeApplication)
	app, err := application.Load(project, config, application.RuntimeMode)
	shared.ExitOnError(err, "<%s>", config.Name)

	// Web server interface to codefly
	if server {
		waitOnServer.Add(1)
		go func() {
			defer waitOnServer.Done()
			m := management.NewManager()
			workspace, err := m.Load()
			shared.ExitOnError(err, "cannot load management")
			w, err := web.NewServer(web.ServerData{Workspace: workspace})
			shared.ExitOnError(err, "cannot create applications server")
			errs <- w.Start(ctx)
		}()
	}

	if initOnly {
		golor.Println(`Press #(italic,white)[Ctrl+C] to exit`)
	} else {
		err = app.Configure(ctx)
		shared.ExitOnError(err, "cannot configure application")

		if !configureOnly {
			go func() {
				errs <- app.Run(ctx)
			}()
		}
	}

	// Listen for errors or context cancellation
loop:
	for {
		select {
		case err := <-errs:
			if err != nil {
				if outputError, ok := shared.IsOutputError(err); ok {
					fmt.Printf("Got output error:\n %v\n", outputError)
					continue
				}
				fmt.Printf("Got applications run error: %v\n", err)
			}
			break loop
		case <-ctx.Done():
			fmt.Println("Got context.Cancel: Exiting...")
			break loop
		}
	}

	// Perform cleanup
	plugins.ClearPlugins()
	if server {
		waitOnServer.Done() // Wait for the server goroutine to finish
	}
	if initOnly || configureOnly {
		return
	}
	logger.Debugf("Stopping application <%s>", config.Name)
	err = app.Stop(ctx)
	shared.ExitOnError(err, "cannot stop application")

}

var (
	server bool
)

func init() {
	ApplicationCmd.Flags().BoolVar(&server, "server", false, "Run application server")
	ApplicationCmd.Flags().BoolVar(&current, "current", false, "Run the current application")
	ApplicationCmd.Flags().BoolVar(&initOnly, "init-only", false, "Run only the application init")
	ApplicationCmd.Flags().BoolVar(&configureOnly, "configure-only", false, "Run only the application configure")
}
