package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

func RenderModule(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *resources.Environment, project string, sink orchestration.OutputSink) (RenderResult, error) {
	destination := filepath.Join(workspace.Dir(), "deployments", "environments", env.Name, "modules", module.Name)
	return RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination,
		Module:      module.Name, Environment: env.Name, AppProject: project,
		Promotable: !env.IsK3d(),
	}, func(ctx context.Context, stage string) error {
		static := filepath.Join(module.Dir(), "deployment", "kustomize")
		if info, err := os.Stat(static); err == nil && info.IsDir() {
			if err := copyTree(static, filepath.Join(stage, "kustomize")); err != nil {
				return fmt.Errorf("copy module kustomize tree: %w", err)
			}
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect module kustomize tree: %w", err)
		}
		for _, reference := range module.ServiceReferences {
			service, err := module.LoadServiceFromName(ctx, reference.Name)
			if err != nil {
				return fmt.Errorf("load service %s: %w", reference.Name, err)
			}
			target := filepath.Join(stage, "services", service.Name)
			if err := renderServiceFlow(ctx, workspace, module, service, env, true, sink, func(_ *resources.Module, _ *resources.Service) string {
				return target
			}); err != nil {
				return fmt.Errorf("render service %s: %w", service.Name, err)
			}
		}
		return nil
	})
}

func RenderService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *resources.Environment, project string, standAlone bool, sink orchestration.OutputSink) (RenderResult, error) {
	destination := filepath.Join(workspace.Dir(), "deployments", "environments", env.Name, "services", module.Name, service.Name)
	return RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination,
		Module:      module.Name, Service: service.Name, Environment: env.Name, AppProject: project,
		Promotable: !env.IsK3d(),
	}, func(ctx context.Context, stage string) error {
		return renderServiceFlow(ctx, workspace, module, service, env, standAlone, sink, serviceRenderDestinations(stage))
	})
}

func serviceRenderDestinations(root string) func(*resources.Module, *resources.Service) string {
	return func(module *resources.Module, service *resources.Service) string {
		return filepath.Join(root, "modules", module.Name, "services", service.Name)
	}
}

func renderServiceFlow(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	service *resources.Service,
	env *resources.Environment,
	standAlone bool,
	sink orchestration.OutputSink,
	destination func(*resources.Module, *resources.Service) string,
) (result error) {
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.DeployMode)
	if err != nil {
		return err
	}
	if sink != nil {
		flow.WithOutputSink(sink)
	}
	flow.WithStandAlone(standAlone)
	defer func() {
		if stopErr := flow.Stop(); result == nil && stopErr != nil {
			result = stopErr
		}
	}()
	if err := flow.InitManagers(ctx); err != nil {
		return err
	}
	if err := flow.Load(ctx); err != nil {
		return err
	}
	flow.WithDeploymentDestination(destination)
	if err := flow.Deploy(ctx); err != nil {
		return err
	}
	return nil
}
