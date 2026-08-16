package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/orchestration"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

func RenderModule(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *resources.Environment, project string, sink orchestration.OutputSink) (RenderResult, error) {
	return renderModuleTree(ctx, workspace, module, env, project, sink, true)
}

func RenderModuleSnapshot(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *resources.Environment, project string, sink orchestration.OutputSink) (RenderResult, error) {
	return renderModuleTree(ctx, workspace, module, env, project, sink, false)
}

func renderModuleTree(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	env *resources.Environment,
	project string,
	sink orchestration.OutputSink,
	includeBootstrap bool,
) (RenderResult, error) {
	destination := filepath.Join(workspace.Dir(), "deployments", "modules", module.Name)
	ownedPath := filepath.ToSlash(filepath.Join("deployments", "modules", module.Name))
	gitopsPath := ""
	if env.Gitops != nil {
		gitopsPath = env.Gitops.Path
	} else if workspace.Gitops != nil {
		gitopsPath = workspace.Gitops.Path
	}
	if gitopsPath != "" {
		ownedPath = filepath.ToSlash(filepath.Join(gitopsPath, ownedPath))
	}
	options := &RenderOptions{
		Destination: destination,
		Module:      module.Name,
		Environment: env.Name,
		Namespace:   env.Namespace,
		AppProject:  project,
		Promotable:  true,
		OwnedPath:   ownedPath,
	}
	return RenderOwnedTree(ctx, options, func(ctx context.Context, stage string) error {
		services := make([]*resources.Service, 0, len(module.ServiceReferences))
		for _, reference := range module.ServiceReferences {
			service, err := module.LoadServiceFromName(ctx, reference.Name)
			if err != nil {
				return fmt.Errorf("load service %s: %w", reference.Name, err)
			}
			services = append(services, service)
		}
		roots, err := moduleRenderRoots(module.Name, services)
		if err != nil {
			return err
		}
		outputs := make(map[string]*builderv0.DeploymentOutput)
		for _, service := range roots {
			if err := renderServiceFlow(
				ctx,
				workspace,
				module,
				service,
				env,
				false,
				sink,
				func(_ *resources.Module, rendered *resources.Service) string {
					serviceDir, _ := unitDirectory(UnitKindService)
					return filepath.Join(stage, serviceDir, rendered.Name)
				},
				func(rendered map[string]*builderv0.DeploymentOutput) {
					for unique, output := range rendered {
						outputs[unique] = output
					}
				},
			); err != nil {
				return fmt.Errorf("render service %s: %w", service.Name, err)
			}
		}
		unitDir, _ := unitDirectory(UnitKindService)
		for _, service := range services {
			_, managed := env.ManagedServices[service.Name]
			entry := InventoryUnit{
				Kind:    UnitKindService,
				Module:  module.Name,
				Name:    service.Name,
				Managed: managed,
			}
			if managed {
				bootstrap, err := retainManagedBootstrap(
					filepath.Join(stage, unitDir, service.Name),
					service.Name,
					env.Name,
				)
				if err != nil {
					return fmt.Errorf("select managed service %s bootstrap output: %w", service.Name, err)
				}
				if bootstrap {
					entry.Path = filepath.ToSlash(filepath.Join(unitDir, service.Name))
					entry.Bootstrap = true
					entry.Output = inventoryKubernetesOutput(outputs[resources.ServiceUnique(module.Name, service.Name)])
				}
			} else {
				entry.Path = filepath.ToSlash(filepath.Join(unitDir, service.Name))
				entry.Output = inventoryKubernetesOutput(outputs[resources.ServiceUnique(module.Name, service.Name)])
				if entry.Output == nil {
					return fmt.Errorf("service %s returned no Kubernetes deployment evidence", service.Name)
				}
			}
			options.Units = append(options.Units, entry)
		}
		sort.Slice(options.Units, func(i, j int) bool {
			return options.Units[i].Name < options.Units[j].Name
		})
		if module.Agent != nil {
			modulePath := filepath.Join(stage, moduleBundleDir)
			if err = renderModuleBundle(ctx, workspace, module, env, modulePath, options.Units); err != nil {
				return err
			}
			options.ModulePath = moduleBundleDir
			return nil
		}
		if !includeBootstrap {
			return nil
		}
		static := filepath.Join(module.Dir(), "deployment", "kustomize")
		info, err := os.Stat(static)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect module kustomize tree: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("module kustomize path is not a directory")
		}
		if err := copyEnvironmentBootstrap(static, env.Name, filepath.Join(stage, "bootstrap")); err != nil {
			return fmt.Errorf("copy module environment bootstrap: %w", err)
		}
		return nil
	})
}

func moduleRenderRoots(module string, services []*resources.Service) ([]*resources.Service, error) {
	members := make(map[string]bool, len(services))
	for _, service := range services {
		members[service.Name] = true
	}
	required := make(map[string]bool, len(services))
	for _, service := range services {
		for _, dependency := range service.ServiceDependencies {
			if (dependency.Module == "" || dependency.Module == module) && members[dependency.Name] {
				required[dependency.Name] = true
			}
		}
	}
	var roots []*resources.Service
	for _, service := range services {
		if !required[service.Name] {
			roots = append(roots, service)
		}
	}
	if len(services) > 0 && len(roots) == 0 {
		return nil, fmt.Errorf("module %s service graph has no entry point", module)
	}
	return roots, nil
}

func copySelectedEnvironmentBootstrap(moduleDir, environment, destination string) (bool, error) {
	static := filepath.Join(moduleDir, "deployment", "kustomize")
	info, err := os.Stat(static)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect module kustomize tree: %w", err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("module kustomize path is not a directory")
	}
	if err := copyEnvironmentBootstrap(
		static,
		environment,
		filepath.Join(destination, "kustomize"),
	); err != nil {
		return false, err
	}
	return true, nil
}

func copyEnvironmentBootstrap(source, environment, destination string) error {
	selected := filepath.Join(source, "overlays", environment)
	info, err := os.Stat(selected)
	if err != nil {
		return fmt.Errorf("select environment overlay %q: %w", environment, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("environment overlay %q is not a directory", environment)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		destinationPath := filepath.Join(destination, entry.Name())
		if entry.Name() == "overlays" {
			sourcePath = selected
			destinationPath = filepath.Join(destinationPath, environment)
		}
		if err := copyTree(sourcePath, destinationPath); err != nil {
			return err
		}
	}
	return nil
}

func RenderService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *resources.Environment, project string, standAlone bool, sink orchestration.OutputSink) (RenderResult, error) {
	serviceDir, _ := unitDirectory(UnitKindService)
	destination := filepath.Join(workspace.Dir(), "deployments", "environments", env.Name, serviceDir, module.Name, service.Name)
	return RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination,
		Module:      module.Name,
		Unit:        service.Name,
		Environment: env.Name,
		Namespace:   env.Namespace,
		AppProject:  project,
		Promotable:  true,
	}, func(ctx context.Context, stage string) error {
		return renderServiceFlow(
			ctx,
			workspace,
			module,
			service,
			env,
			standAlone,
			sink,
			serviceRenderDestinations(stage),
			nil,
		)
	})
}

func serviceRenderDestinations(root string) func(*resources.Module, *resources.Service) string {
	serviceDir, _ := unitDirectory(UnitKindService)
	return func(module *resources.Module, service *resources.Service) string {
		return filepath.Join(root, "modules", module.Name, serviceDir, service.Name)
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
	record func(map[string]*builderv0.DeploymentOutput),
) (result error) {
	if env.Registry == nil || strings.TrimSpace(env.Registry.URL) == "" {
		return fmt.Errorf("environment %s must declare registry.url for an immutable GitOps snapshot", env.Name)
	}
	builder.SetRepository(env.Registry.URL)
	if env.Cluster != nil && env.Cluster.Kind == "k3d" {
		if env.Registry.Auth != "" {
			if err := builder.RegistryLogin(ctx, env.Registry.URL, env.Registry.Auth); err != nil {
				return fmt.Errorf("authenticate snapshot registry: %w", err)
			}
		}
		orchestration.SetBuilderPush()
	}
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.SnapshotMode)
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
	flow.WithDeploymentManager(gitOpsDeploymentOutputManager{})
	flow.WithDeploymentDestination(destination)
	flow.WithKubernetesOutputProfile(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
	)
	if err := flow.Deploy(ctx); err != nil {
		return err
	}
	if record != nil {
		record(flow.DeploymentOutputs())
	}
	return nil
}

type gitOpsDeploymentOutputManager struct{}

func (gitOpsDeploymentOutputManager) RequiresDeploymentOutput() bool {
	return true
}

func (gitOpsDeploymentOutputManager) Handle(
	context.Context,
	*resources.Service,
	*resources.Module,
	*builderv0.DeploymentOutput,
) error {
	return nil
}
