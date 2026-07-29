package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/orchestration"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

func RenderModule(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *resources.Environment, project string, sink orchestration.OutputSink) (RenderResult, error) {
	destination := filepath.Join(workspace.Dir(), "deployments", "modules", module.Name)
	managed, err := selectedManagedServices(workspace, env.Name)
	if err != nil {
		return RenderResult{}, err
	}
	services := make([]string, 0, len(module.ServiceReferences))
	serviceGraph := make([]InventoryService, 0, len(module.ServiceReferences))
	declared := make(map[string]struct{}, len(module.ServiceReferences))
	for _, reference := range module.ServiceReferences {
		declared[reference.Name] = struct{}{}
		_, isManaged := managed[reference.Name]
		service := InventoryService{Module: module.Name, Service: reference.Name, Managed: isManaged}
		if !isManaged {
			services = append(services, reference.Name)
			service.Path = filepath.ToSlash(filepath.Join("services", reference.Name))
		}
		serviceGraph = append(serviceGraph, service)
	}
	for service := range managed {
		if _, exists := declared[service]; !exists {
			return RenderResult{}, fmt.Errorf("managed service %q is outside module %q", service, module.Name)
		}
	}
	ownedPath := filepath.ToSlash(filepath.Join("deployments", "modules", module.Name))
	if workspace.Gitops != nil {
		ownedPath = filepath.ToSlash(filepath.Join(workspace.Gitops.Path, ownedPath))
	}
	options := &RenderOptions{
		Destination: destination,
		Module:      module.Name, Services: services, OwnedPath: ownedPath, ServiceGraph: serviceGraph,
		Environment: env.Name, AppProject: project,
		Promotable: true,
	}
	return RenderOwnedTree(ctx, options, func(ctx context.Context, stage string) error {
		static := filepath.Join(module.Dir(), "deployment", "kustomize")
		if info, err := os.Stat(static); err == nil && info.IsDir() {
			if err := copyEnvironmentBootstrap(static, env.Name, filepath.Join(stage, "bootstrap")); err != nil {
				return fmt.Errorf("copy module environment bootstrap: %w", err)
			}
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect module kustomize tree: %w", err)
		}
		for _, reference := range module.ServiceReferences {
			if _, isManaged := managed[reference.Name]; isManaged {
				continue
			}
			service, err := module.LoadServiceFromName(ctx, reference.Name)
			if err != nil {
				return fmt.Errorf("load service %s: %w", reference.Name, err)
			}
			target := filepath.Join(stage, "services", service.Name)
			output, err := renderServiceFlow(ctx, workspace, module, service, env, true, sink, func(_ *resources.Module, _ *resources.Service) string {
				return target
			})
			if err != nil {
				return fmt.Errorf("render service %s: %w", service.Name, err)
			}
			for index := range options.ServiceGraph {
				if options.ServiceGraph[index].Service == service.Name {
					options.ServiceGraph[index].Output = kubernetesOutputInventory(output)
					break
				}
			}
		}
		return nil
	})
}

func selectedManagedServices(workspace *resources.Workspace, environment string) (map[string]struct{}, error) {
	data, err := os.ReadFile(filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName))
	if err != nil {
		return nil, err
	}
	var document struct {
		Environments []struct {
			Name            string         `yaml:"name"`
			ManagedServices map[string]any `yaml:"managed-services"`
		} `yaml:"environments"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode managed service graph: %w", err)
	}
	managed := map[string]struct{}{}
	for _, candidate := range document.Environments {
		if candidate.Name != environment {
			continue
		}
		for service := range candidate.ManagedServices {
			managed[service] = struct{}{}
		}
		break
	}
	return managed, nil
}

func kubernetesOutputInventory(output *builderv0.KubernetesDeploymentOutput) *KubernetesOutputInventory {
	if output == nil {
		return nil
	}
	validation := output.GetValidation()
	violations := append([]string{}, validation.GetViolations()...)
	return &KubernetesOutputInventory{
		Kind:            output.GetKind().String(),
		Profile:         output.GetProfile().String(),
		ContractVersion: output.GetContractVersion(),
		Validation: KubernetesValidationInventory{
			StaticValidation:     validation.GetStaticValidation().String(),
			ServerSideValidation: validation.GetServerSideValidation().String(),
			Promotable:           validation.GetPromotable(),
			Violations:           violations,
		},
	}
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
	return copyTree(selected, destination)
}

func RenderService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *resources.Environment, project string, standAlone bool, sink orchestration.OutputSink) (RenderResult, error) {
	destination := filepath.Join(workspace.Dir(), "deployments", "environments", env.Name, "services", module.Name, service.Name)
	return RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination,
		Module:      module.Name, Service: service.Name, Environment: env.Name, AppProject: project,
		Promotable: true,
	}, func(ctx context.Context, stage string) error {
		_, err := renderServiceFlow(ctx, workspace, module, service, env, standAlone, sink, serviceRenderDestinations(stage))
		return err
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
) (_ *builderv0.KubernetesDeploymentOutput, result error) {
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.DeployMode)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if err := flow.Load(ctx); err != nil {
		return nil, err
	}
	capture := &deploymentOutputCapture{}
	flow.WithDeploymentManager(capture)
	flow.WithDeploymentDestination(destination)
	if err := flow.Deploy(ctx); err != nil {
		return nil, err
	}
	return capture.output, nil
}

type deploymentOutputCapture struct {
	output *builderv0.KubernetesDeploymentOutput
}

func (capture *deploymentOutputCapture) Handle(
	_ context.Context,
	_ *resources.Service,
	_ *resources.Module,
	output *builderv0.DeploymentOutput,
) error {
	kubernetes := output.GetKubernetes()
	if kubernetes == nil {
		return fmt.Errorf("plugin returned no Kubernetes deployment output")
	}
	capture.output = proto.Clone(kubernetes).(*builderv0.KubernetesDeploymentOutput)
	return nil
}
