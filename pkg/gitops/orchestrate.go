package gitops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	options := &RenderOptions{
		Destination: destination,
		Module:      module.Name, Environment: env.Name, AppProject: project,
		Promotable: true,
		OwnedPath:  filepath.ToSlash(filepath.Join("deployments", "modules", module.Name)),
	}
	return RenderOwnedTree(ctx, options, func(ctx context.Context, stage string) error {
		var services []*resources.Service
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
			if err := renderServiceFlow(ctx, workspace, module, service, env, false, sink, func(_ *resources.Module, rendered *resources.Service) string {
				return filepath.Join(stage, "services", rendered.Name)
			}, func(rendered map[string]*builderv0.DeploymentOutput) {
				for unique, output := range rendered {
					outputs[unique] = output
				}
			}); err != nil {
				return fmt.Errorf("render service %s: %w", service.Name, err)
			}
		}
		for _, service := range services {
			_, managed := env.ManagedServices[service.Name]
			entry := InventoryService{
				Module:  module.Name,
				Service: service.Name,
				Managed: managed,
			}
			if managed {
				if err := os.RemoveAll(filepath.Join(stage, "services", service.Name)); err != nil {
					return fmt.Errorf("remove managed service %s output: %w", service.Name, err)
				}
			} else {
				entry.Path = filepath.ToSlash(filepath.Join("services", service.Name))
				entry.Output = inventoryKubernetesOutput(outputs[resources.ServiceUnique(module.Name, service.Name)])
				if entry.Output == nil {
					return fmt.Errorf("service %s returned no Kubernetes deployment evidence", service.Name)
				}
			}
			options.ServiceGraph = append(options.ServiceGraph, entry)
		}
		sort.Slice(options.ServiceGraph, func(i, j int) bool {
			return options.ServiceGraph[i].Service < options.ServiceGraph[j].Service
		})
		if !includeBootstrap {
			return nil
		}
		return generateEnvironmentBootstrap(ctx, workspace, module, env.Name, stage)
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

func generateEnvironmentBootstrap(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	environment,
	destination string,
) error {
	if module.Agent == nil {
		_, err := copySelectedEnvironmentBootstrap(module.Dir(), environment, destination)
		return err
	}
	binary, err := module.Agent.Path(ctx)
	if err != nil {
		return fmt.Errorf("resolve module generator %s: %w", module.Agent.Identifier(), err)
	}
	target := filepath.Join(destination, "kustomize")
	command := exec.CommandContext(
		ctx,
		binary,
		"gitops",
		module.Dir(),
		workspace.Dir(),
		environment,
		target,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"generate %s module bootstrap with %s: %w: %s",
			environment,
			module.Agent.Identifier(),
			err,
			strings.TrimSpace(string(output)),
		)
	}
	return nil
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
	environmentBootstrap := filepath.Join(static, "overlays", environment)
	info, err = os.Stat(environmentBootstrap)
	if err != nil {
		return false, fmt.Errorf("inspect generated %s module bootstrap: %w", environment, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("generated %s module bootstrap is not a directory", environment)
	}
	if err := copyTree(
		environmentBootstrap,
		filepath.Join(destination, "kustomize", "overlays", environment),
	); err != nil {
		return false, fmt.Errorf("copy generated %s module bootstrap: %w", environment, err)
	}
	return true, nil
}

func RenderService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *resources.Environment, project string, standAlone bool, sink orchestration.OutputSink) (RenderResult, error) {
	destination := filepath.Join(workspace.Dir(), "deployments", "environments", env.Name, "services", module.Name, service.Name)
	return RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination,
		Module:      module.Name, Service: service.Name, Environment: env.Name, AppProject: project,
		Promotable: true,
	}, func(ctx context.Context, stage string) error {
		return renderServiceFlow(ctx, workspace, module, service, env, standAlone, sink, serviceRenderDestinations(stage), nil)
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
