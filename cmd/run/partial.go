package run

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/overview"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/management"
	"github.com/codefly-dev/cli/pkg/partial"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// PartialCmd represents the run command
var PartialCmd = &cobra.Command{
	Use:   "partial",
	Short: "Run an partial",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		logger := shared.GetLogger(ctx).With("run.PartialCmd")
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		errs := make(chan error)

		if len(args) == 0 {
			fmt.Println("Please provide a partial name")
			os.Exit(0)
		}
		name := args[0]

		project := common.Project(ctx)

		conf, err := project.GetPartial(name)
		if err != nil {
			fmt.Printf("Cannot find partial <%s> in project <%s>\n", name, project.Name)
			os.Exit(1)
		}

		partial, err := partial.NewPartial(ctx, project, conf, application.RuntimeMode)
		shared.UnexpectedExitOnError(err, "<%s>", conf.Name)

		session := management.NewPartialSession(conf)
		spy, err := management.NewSpy(ctx, session)
		shared.ExitOnError(err, "cannot create spy")
		defer spy.Close()

		// Web server interface to codefly
		if server {
			m := management.NewManager()
			workspace, err := m.Load()
			shared.ExitOnError(err, "cannot load management")

			dependencyGraph, err := overview.NewDependencyGraph(project)
			shared.ExitOnError(err, "cannot load management")

			w, err := web.NewServer(web.ServerData{Workspace: workspace, DependencyGraph: dependencyGraph})

			shared.ExitOnError(err, "cannot create applications server")
			go func() {
				errs <- w.Start(ctx)
			}()
		}

		if initOnly {
			goto cleanup
		}
		err = partial.Configure(ctx)
		shared.UnexpectedExitOnError(err, "cannot configure partial")

		if configureOnly {
			goto cleanup
		}

		err = spy.Activate(ctx)
		shared.UnexpectedExitOnError(err, "cannot activate spy")

		go func() {
			errs <- partial.Run(ctx)
		}()

		for {
			select {
			case err := <-errs:
				if err == nil {
					fmt.Println("Name exited successfully")
					goto cleanup
				}
				// Do we have some output error
				if ok, err := shared.IsOutputError(err); ok {
					fmt.Printf("Got output error:\n %v\n", err)
					continue
				}
				fmt.Printf("Got partials run error: %v\n", err)
				goto cleanup
			case <-ctx.Done():
				fmt.Println("Got context.Cancel: Exiting...")
				goto cleanup

			}
		}
	cleanup:
		logger.Debugf("Clearing agents")
		agents.ClearAgents()
		fmt.Println("Cleaning up...")
		if initOnly {
			return
		}
		//logger.Debugf("Stopping partial <%s>", conf.Name)
		//err = partial.Stop(ctx)
		//shared.ExitOnError(err, "cannot stop partial")
	},
}

func init() {
	PartialCmd.Flags().BoolVar(&server, "server", false, "Run the web server")
	PartialCmd.Flags().BoolVar(&current, "current", false, "Run the current application")
	PartialCmd.Flags().BoolVar(&initOnly, "init-only", false, "Run only the application init step")
	PartialCmd.Flags().BoolVar(&configureOnly, "configure-only", false, "Run only the application configure step")
}
