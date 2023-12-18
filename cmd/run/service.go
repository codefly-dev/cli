package run

import (
	"context"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/display"
	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Run a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		app := common.Application(ctx)
		if app == nil {
			cli.Error("No application found where you are running the command: use the name of the service as argument")
		}
		// Without argument, run the current service
		var service *configurations.Service
		var err error
		if len(args) == 0 {
			service = common.Service(ctx)
			if service == nil {
				cli.Error("No service found where you are running the command: use the name of the service as argument")
				os.Exit(0)
			}
		} else {
			shared.GetLogger(ctx).TODO("WE NEED TO HAVE THE FULL GRAPH OF PROJECTS/APPLICATIONS/SERVICES")
			service, err = app.LoadServiceFromName(ctx, args[0])
			shared.ExitOnError(err, "cannot load service <%s>", args[0])
		}

		cli.Header(2, "Running service <{{.Service.Name}}> in application <{{.Application.Name}}>",
			display.New().WithApplication(app).WithService(service))

		err = runService(ctx, service)
		shared.ExitOnError(err, "cannot run service <%s>", service.Name)
	},
}

func runService(ctx context.Context, service *configurations.Service) error {
	return services.Run(ctx, service)

	//	w := wool.Get(ctx).In("run.ServiceCmd")
	//
	//	// Create a context that is cancelled on os.Interrupt or os.Kill
	//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	//	defer stop()
	//
	//	var waitOnServer sync.WaitGroup
	//	errs := make(chan error, 1) // Buffered channel
	//
	//	configurations.SetMode(configurations.ModeService)
	//	app, err := service.Load(ctx, project, config, service.RuntimeMode)
	//	shared.ExitOnError(err, "<%s>", config.Name)
	//
	//	// Web server interface to codefly
	//	if server {
	//		waitOnServer.Add(1)
	//		go func() {
	//			defer waitOnServer.Done()
	//
	//			m := management.NewManager()
	//			workspace, err := m.Load()
	//			shared.ExitOnError(err, "cannot load management")
	//
	//			dependencyGraph, err := overview.NewDependencyGraph(project)
	//			shared.ExitOnError(err, "cannot load management")
	//
	//			w, err := web.NewServer(web.ServerData{Workspace: workspace, DependencyGraph: dependencyGraph})
	//			shared.ExitOnError(err, "cannot create services server")
	//			errs <- w.Start(ctx)
	//		}()
	//	}
	//
	//	if initOnly {
	//		golor.Println(`Press #(italic,white)[Ctrl+C] to exit`)
	//	} else {
	//		err = app.Configure(ctx)
	//		shared.ExitOnError(err, "cannot configure service")
	//
	//		if !configureOnly {
	//			go func() {
	//				errs <- app.Run(ctx)
	//			}()
	//		}
	//	}
	//
	//	// Listen for errors or context cancellation
	//loop:
	//	for {
	//		select {
	//		case err := <-errs:
	//			if err != nil {
	//				if ok, err := shared.IsOutputError(err); ok {
	//					fmt.Printf("Got output error:\n %v\n", err)
	//					continue
	//				}
	//				fmt.Printf("Got services run error: %v\n", err)
	//			}
	//			break loop
	//		case <-ctx.Done():
	//			fmt.Println("Got context.Cancel: Exiting...")
	//			break loop
	//		}
	//	}
	//
	//	// Perform cleanup
	//	agents.ClearAgents()
	//	if server {
	//		waitOnServer.Done() // Wait for the server goroutine to finish
	//	}
	//	if initOnly || configureOnly {
	//		return
	//	}
	//	logger.Debugf("Stopping service <%s>", config.Name)
	//	err = app.Stop(ctx)
	//	shared.ExitOnError(err, "cannot stop service")

}

func init() {
	ServiceCmd.Flags().BoolVar(&withServer, "server", false, "Run service server")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Run only the service init")
	ServiceCmd.Flags().BoolVar(&configureOnly, "configure-only", false, "Run only the service configure")
}
