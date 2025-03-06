package run

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/web"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Run a service",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
		defer stop()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		var workspace *resources.Workspace
		var module *resources.Module
		var service *resources.Service

		if servicePath != "" {
			workspace, module, service = common.LoadWithServicePathOverride(ctx, servicePath)
		} else {
			workspace, module, service = common.LoadRequired(ctx, args)
		}

		common.WithSilence(ctx, workspace, silent)

		errs := make(chan error, 1) // Buffered channel

		if withCLIServer {
			server, err := web.NewServer(web.ServerData{Workspace: workspace})
			cli.ExitOnError(err, "cannot create web server")
			go func() {
				errs <- server.Start(ctx)
			}()
		}

		flow, err := initRunService(ctx, workspace, module, service)
		if err != nil {
			err = errors.Unwrap(err)
			cli.ExitOnError(err, "Cannot init flow")
		}
		go func() {
			errs <- runService(ctx, flow)
		}()

	loop:
		for {
			select {
			case err := <-errs:
				if err != nil {
					cli.Error("Got service run error: %v\n", errors.Unwrap(err))
					_ = stopService(ctx, flow)
					cli.ExitOnError(err, "Cannot stop flow")
				}
				errs <- nil
				break loop
			case <-ctx.Done():
				cli.Header(2, "Got context.Cancel: Exiting...")
				break loop
			}
		}
		stopped := <-errs
		if stopped != nil {
			cli.Error("Got error while stopping service: %v", errors.Unwrap(stopped))
		}
		err = stopService(ctx, flow)
		cli.ExitOnError(err, "Cannot stop flow")
		cli.Header(1, "Work done!")
		cli.Exit()
	},
}

func initRunService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("runService", wool.ThisField(resources.WithUnique(service)))
	// Catch panic
	defer w.Catch()

	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return nil, w.NewError("Invalid runtime context: %s", runtimeContext)
	}

	env := resources.LocalEnvironment()
	// Setup optional naming namingScope
	env.NamingScope = namingScope

	// Parse remote services
	remoteServices, err := parseRemote(workspace, remotes)
	if err != nil {
		return nil, w.Wrap(err)
	}
	w.Info("Running with remotes", wool.Field("remotes", remoteServices))

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.RunMode)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow.WithLoadOnly(loadOnly)
	flow.WithInitOnly(initOnly)
	flow.WithOutputEnv(outputEnv)
	flow.WithStandAlone(standAlone)
	flow.WithExcludeRoot(excludeRoot)
	flow.WithRuntimeContext(runtimeContext)
	flow.WithFixture(fixture)
	flow.WithRemotes(remoteServices)
	flow.WithStopAfter(stopAfter)
	err = flow.InitManagers(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	err = flow.Load(ctx)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return flow, nil
}

func parseRemote(workspace *resources.Workspace, remotes []string) ([]*orchestration.Remote, error) {
	var out []*orchestration.Remote
	// Remote should be unique-ish:env
	for _, remote := range remotes {
		tokens := strings.Split(remote, ":")
		if len(tokens) != 2 {
			return nil, errors.New("Remote should be in the format: service:env")
		}
		serviceWithModule, err := resources.ParseServiceWithOptionalModule(tokens[0])
		if err != nil {
			return nil, err
		}
		// Need to check if we know this environment
		env := &resources.Environment{Name: tokens[1]}
		out = append(out, &orchestration.Remote{ServiceWithModule: serviceWithModule, Environment: env})
	}
	return out, nil
}

func runService(ctx context.Context, flow *orchestration.Flow) error {
	// Catch panic
	w := wool.Get(ctx).In("runService")
	defer w.Catch()
	err := flow.Start(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil
}

func stopService(ctx context.Context, flow *orchestration.Flow) error {
	// Catch panic
	w := wool.Get(ctx).In("stopService")
	defer w.Catch()
	if flow == nil {
		return nil
	}
	w.Info("Stopping services")
	err := flow.Stop()
	if err != nil {
		return w.Wrapf(err, "cannot stop service")
	}
	return nil
}

func init() {
	ServiceCmd.Flags().BoolVar(&withCLIServer, "cli-server", false, "Start CLI server")
	ServiceCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for the flow")
	ServiceCmd.Flags().StringVar(&namingScope, "naming-scope", "", "Runtime namingScope (for testing encapsulation)")
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().StringVar(&servicePath, "service-path", "", "Path to the service")
	ServiceCmd.Flags().StringVar(&outputEnv, "output-env", "", "Output environment variables")
	ServiceCmd.Flags().BoolVar(&excludeRoot, "exclude-root", false, "Exclude root service")
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Initialize service only, i.e. without running it")
	ServiceCmd.Flags().BoolVar(&loadOnly, "load-only", false, "LoadRequired service only, i.e. without running it")
	ServiceCmd.Flags().StringSliceVar(&silent, "silent", nil, "Silence services in CLI output")
	ServiceCmd.Flags().StringVar(&fixture, "fixture", "", "Fixture to use for the service")
	ServiceCmd.Flags().StringSliceVar(&remotes, "remote", nil, "Remote services")
	ServiceCmd.Flags().StringVar(&stopAfter, "stop-after", "", "Stop after a specific action")
}
