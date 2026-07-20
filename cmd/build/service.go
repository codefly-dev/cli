package build

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the build command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Build a service",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.RegisterCleanup(services.ClearAgents)

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}

		flow, err := initBuildService(ctx, workspace, module, service, standAlone)
		if err != nil {
			return fmt.Errorf("cannot initialize service: %w", err)
		}
		cleaned := false
		cleanup := func() error {
			if cleaned {
				return nil
			}
			cleaned = true
			return cleanBuildService(flow)
		}
		defer func() { _ = cleanup() }()

		buildErr := common.WithHeartbeat(ctx, "building "+service.Name, func() error {
			return buildService(ctx, flow)
		})
		stopErr := cleanup()
		var result []error
		if buildErr != nil {
			result = append(result, fmt.Errorf("service build failed: %w", buildErr))
		}
		if stopErr != nil {
			result = append(result, fmt.Errorf("cannot stop flow: %w", stopErr))
		}
		if len(result) > 0 {
			return errors.Join(result...)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Work done!")
		return nil
	},
}

func initBuildService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, standAlone bool) (*orchestration.Flow, error) {
	w := wool.Get(ctx).In("buildService", wool.ThisField(resources.WithUnique(service)))

	// Resolve the target environment so we can pick up its registry,
	// namespace, etc.
	env, err := orchestration.SelectEnvironment(workspace, envInput)
	if err != nil {
		return nil, w.Wrap(err)
	}

	// Image-registry resolution order:
	//   1. --org flag (explicit override; wins everything).
	//   2. env.Registry.URL declared in workspace.codefly.yaml.
	//   3. legacy default (the existing hardcoded ECR URL, kept for
	//      back-compat until every workspace declares an env).
	var registryURL string
	switch {
	case org != "":
		registryURL = org
	case env.Registry != nil && env.Registry.URL != "":
		registryURL = env.Registry.URL
	}
	if registryURL != "" {
		builder.SetRepository(registryURL)
	}

	if push {
		// Authenticate against the registry before the build runs.
		// Skip when we don't have a registry to push to (anonymous
		// build), or when the env doesn't declare an Auth method
		// (assume pre-existing docker creds in ~/.docker/config.json).
		if registryURL != "" && env.Registry != nil && env.Registry.Auth != "" {
			if err := builder.RegistryLogin(ctx, registryURL, env.Registry.Auth); err != nil {
				return nil, w.Wrapf(err, "registry login failed")
			}
		}
		orchestration.SetBuilderPush()
	}
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.BuildMode)
	if err != nil {
		return nil, w.Wrap(err)
	}
	flow.WithStandAlone(standAlone)
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

func cleanBuildService(flow *orchestration.Flow) error {
	defer services.ClearAgents()
	return flow.Stop()
}

func buildService(ctx context.Context, flow *orchestration.Flow) error {
	w := wool.Get(ctx).In("buildService")
	err := flow.Build(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot start service")
	}
	return nil

}

var standAlone bool
var org string
var push bool
var envInput string

func init() {
	ServiceCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin service as standalone, i.e. without its dependencies")
	ServiceCmd.Flags().StringVar(&org, "org", "", "Image registry override (e.g. ghcr.io/myorg). Wins over the env's registry.url.")
	ServiceCmd.Flags().BoolVar(&push, "push", false, "Push the image to the repository")
	ServiceCmd.Flags().StringVar(&envInput, "env", "local", "Environment to build for (looks up registry/cluster from workspace.codefly.yaml)")
}
