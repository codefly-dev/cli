package build

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ModuleCmd builds every service in a module. Mirrors `deploy module`
// — one Flow per service, --stand-alone so the module loop is the
// dependency walker. Same env / registry / push semantics as the
// service variant.
var ModuleCmd = &cobra.Command{
	Use:   "module [name]",
	Short: "Build every service in a module",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		cli.RegisterCleanup(services.ClearAgents)

		workspace, module := loadModule(ctx, args)

		env := workspace.FindEnvironment(envInput)
		if env == nil {
			env = &resources.Environment{Name: envInput}
		}

		// Same registry resolution as service.go: --org wins, env
		// fallback, otherwise legacy default. ECR auth runs once
		// up-front instead of per-service so we don't re-login N times.
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
			if registryURL != "" && env.Registry != nil && env.Registry.Auth != "" {
				cli.ExitOnError(builder.RegistryLogin(ctx, registryURL, env.Registry.Auth), "registry login failed")
			}
			orchestration.SetBuilderPush()
		}

		cli.Header(1, "Building module %s for env %s", module.Name, env.Name)

		for _, ref := range module.ServiceReferences {
			cli.Header(2, "Building service %s", ref.Name)
			if err := buildOneService(ctx, workspace, module, ref.Name, env); err != nil {
				cli.ExitOnError(err, "Cannot build service %s", ref.Name)
			}
		}

		cli.Header(1, "Module build done!")
	},
}

func loadModule(ctx context.Context, args []string) (*resources.Workspace, *resources.Module) {
	if len(args) == 0 {
		workspace := common.RequireWorkspace(ctx)
		module := common.RequireModule(ctx)
		return workspace, module
	}
	workspace, err := resources.FindWorkspaceUp(ctx)
	cli.ExitOnError(err, "Cannot find workspace")
	if workspace == nil {
		cli.ExitWithMessage("No workspace found")
	}
	module, err := workspace.LoadModuleFromName(ctx, args[0])
	cli.ExitOnError(err, "Cannot load module %s", args[0])
	return workspace, module
}

func buildOneService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, name string, env *resources.Environment) error {
	w := wool.Get(ctx).In("buildModule.buildOneService", wool.NameField(name))

	service, err := module.LoadServiceFromName(ctx, name)
	if err != nil {
		return w.Wrapf(err, "cannot load service")
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.BuildMode)
	if err != nil {
		return w.Wrap(err)
	}
	flow.WithStandAlone(true)
	if err := flow.InitManagers(ctx); err != nil {
		return w.Wrapf(err, "cannot initialize managers")
	}
	if err := flow.Load(ctx); err != nil {
		return w.Wrap(err)
	}
	if err := flow.Build(ctx); err != nil {
		_ = flow.Stop()
		return w.Wrapf(err, "build failed")
	}
	return flow.Stop()
}

func init() {
	ModuleCmd.Flags().BoolVar(&standAlone, "stand-alone", false, "Begin services as standalone, i.e. without their dependencies")
	ModuleCmd.Flags().StringVar(&org, "org", "", "Image registry override (wins over env's registry.url)")
	ModuleCmd.Flags().BoolVar(&push, "push", false, "Push the images to the registry")
	ModuleCmd.Flags().StringVar(&envInput, "env", "local", "Environment to build for (looks up registry/cluster from workspace.codefly.yaml)")
}
