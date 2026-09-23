package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/codefly-dev/cli/pkg/orchestration"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

func RenderModule(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *environments.Environment, project string, sink orchestration.OutputSink) (RenderResult, error) {
	return renderModuleTree(ctx, workspace, module, env, project, sink, true, false)
}

func RenderModuleSnapshot(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *environments.Environment, project string, sink orchestration.OutputSink) (RenderResult, error) {
	return renderModuleTree(ctx, workspace, module, env, project, sink, false, false)
}

func renderModuleTree(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	env *environments.Environment,
	project string,
	sink orchestration.OutputSink,
	includeBootstrap bool,
	validateCluster bool,
) (RenderResult, error) {
	if err := selectionguard.RejectUnboundExecution(workspace.Dir(), module.Dir()); err != nil {
		return RenderResult{}, err
	}
	if err := environments.ValidateWorkspace(ctx, workspace); err != nil {
		return RenderResult{}, err
	}
	destination := filepath.Join(workspace.Dir(), "deployments", "modules", module.Name)
	ownedPath := filepath.ToSlash(filepath.Join("deployments", "modules", module.Name))
	gitopsPath := ""
	if env.Gitops != nil {
		gitopsPath = env.Gitops.Path
	} else if defaults, err := environments.WorkspaceGitops(workspace); err != nil {
		return RenderResult{}, err
	} else if defaults != nil {
		gitopsPath = defaults.Path
	}
	if gitopsPath != "" {
		ownedPath = filepath.ToSlash(filepath.Join(gitopsPath, ownedPath))
	}
	// The module's namespace, not the environment's: a workspace composing
	// several modules gives each its own, and the render record, the Argo
	// destinations derived from it, the quota and the secret projections all
	// take it from here.
	scope := moduleScope(env, workspace, module.Name)
	options := &RenderOptions{
		Destination: destination,
		Module:      module.Name,
		Environment: env.Name,
		Namespace:   scope.Namespace,
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
		pkg, err := modulePackage(module.Dir())
		if err != nil {
			return err
		}
		options.Package = pkg
		catalog, err := loadContractCatalog(module.Dir())
		if err != nil {
			return err
		}
		roots, err := moduleRenderRoots(module.Name, services)
		if err != nil {
			return err
		}
		// The registry is only needed to build and push service images. A module
		// with no buildable services still renders its bootstrap kustomize tree or
		// module bundle, which need no registry, so demand one only when there is a
		// service to build.
		if len(roots) > 0 {
			if err = prepareSnapshotRegistry(ctx, env); err != nil {
				return err
			}
		}
		outputs := make(map[string]*builderv0.DeploymentOutput)
		selfEndpoints := make(map[string]map[string]string)
		destinations := moduleStageDestinations(workspace, module, stage)
		for _, service := range roots {
			if err := renderServiceFlow(
				ctx,
				workspace,
				module,
				service,
				env,
				false,
				validateCluster,
				sink,
				destinations,
				func(rendered map[string]*builderv0.DeploymentOutput) {
					for unique, output := range rendered {
						outputs[unique] = output
					}
				},
				nil,
				func(rendered map[string]map[string]string) {
					for unique, variables := range rendered {
						selfEndpoints[unique] = variables
					}
				},
			); err != nil {
				return fmt.Errorf("render service %s: %w", service.Name, err)
			}
		}
		injections, err := deriveRenderInjections(ctx, workspace, selfEndpoints, sink)
		if err != nil {
			return err
		}
		unitDir, _ := unitDirectory(UnitKindService)
		for _, service := range services {
			managedService, managed := env.ManagedServices[service.Name]
			entry := InventoryUnit{
				Kind:      UnitKindService,
				Module:    module.Name,
				Name:      service.Name,
				Managed:   managed,
				Contracts: catalog.exposedContracts(module.Name, service.Name),
			}
			if managed {
				bootstrap, bundleErr := retainManagedBundle(
					filepath.Join(stage, unitDir, service.Name),
					service.Name,
					env.Name,
					scope.Namespace,
					managedService.SecretReferences,
				)
				if bundleErr != nil {
					return fmt.Errorf("select managed service %s bundle: %w", service.Name, bundleErr)
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
				if projectErr := projectServiceConfiguration(
					ctx,
					filepath.Join(stage, unitDir, service.Name),
					service,
					env,
					scope,
					injections.forService(module.Name, service.Name),
				); projectErr != nil {
					return projectErr
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
		bootstrap := filepath.Join(stage, "bootstrap")
		if err := copyEnvironmentBootstrap(static, env.Name, bootstrap); err != nil {
			return fmt.Errorf("copy module environment bootstrap: %w", err)
		}
		if _, err := projectResourceQuota(bootstrap, env.Name, scope.Namespace, env.ResourceQuota); err != nil {
			return fmt.Errorf("project module namespace resource quota: %w", err)
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

func RenderService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *environments.Environment, project string, standAlone bool, sink orchestration.OutputSink) (RenderResult, error) {
	return renderService(ctx, workspace, module, service, env, project, standAlone, false, sink)
}

func renderService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *environments.Environment, project string, standAlone, validateCluster bool, sink orchestration.OutputSink) (RenderResult, error) {
	if err := environments.ValidateWorkspace(ctx, workspace); err != nil {
		return RenderResult{}, err
	}
	serviceDir, _ := unitDirectory(UnitKindService)
	destination := filepath.Join(workspace.Dir(), "deployments", "environments", env.Name, serviceDir, module.Name, service.Name)
	pkg, err := modulePackage(module.Dir())
	if err != nil {
		return RenderResult{}, err
	}
	return RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination,
		Module:      module.Name,
		Unit:        service.Name,
		Environment: env.Name,
		Namespace:   env.ModuleNamespace(workspace, module.Name),
		AppProject:  project,
		Promotable:  true,
		Package:     pkg,
	}, func(ctx context.Context, stage string) error {
		if err := prepareSnapshotRegistry(ctx, env); err != nil {
			return err
		}
		var graph map[string]*resources.Service
		var selfEndpoints map[string]map[string]string
		if err := renderServiceFlow(
			ctx,
			workspace,
			module,
			service,
			env,
			standAlone,
			validateCluster,
			sink,
			serviceRenderDestinations(stage),
			nil,
			func(services map[string]*resources.Service) { graph = services },
			func(rendered map[string]map[string]string) { selfEndpoints = rendered },
		); err != nil {
			return err
		}
		injections, err := deriveRenderInjections(ctx, workspace, selfEndpoints, sink)
		if err != nil {
			return err
		}
		return projectRenderedServiceConfiguration(ctx, stage, workspace, env, graph, injections)

	})
}

// moduleStageDestinations locates the staged tree of every service a module
// render deploys. Rendering a module drives the whole dependency graph, so the
// flow also deploys services belonging to other modules, and a destination
// keyed by service name alone makes two modules that ship a same-named service
// resolve to one directory. A service's identity is workspace, module and
// service name together — the host module's "store" and another module's
// "store" are two services, in two namespaces, with two databases — so the
// destination is keyed by all three and they can never share a directory.
//
// The module under render keeps the committed layout "services/<name>": the
// workspace and module halves of its key are already carried by the owned
// tree's own location, <workspace>/deployments/modules/<module>, and the
// inventory contract pins that relative path (see validateInventoryUnits).
// Every other module's services stage outside the owned tree, under the
// render's scratch root, so they cannot collide with the module's own units,
// cannot reach the inventory or the committed tree, and are discarded with the
// staging directory. Their manifests belong to their own module's render.
func moduleStageDestinations(workspace *resources.Workspace, module *resources.Module, owned string) func(*resources.Module, *resources.Service) string {
	serviceDir, _ := unitDirectory(UnitKindService)
	scratch := filepath.Join(filepath.Dir(owned), graphStageDir, workspace.Name)
	return func(rendered *resources.Module, service *resources.Service) string {
		if rendered != nil && rendered.Name == module.Name {
			return filepath.Join(owned, serviceDir, service.Name)
		}
		foreign := ""
		if rendered != nil {
			foreign = rendered.Name
		}
		return filepath.Join(scratch, foreign, serviceDir, service.Name)
	}
}

func serviceRenderDestinations(root string) func(*resources.Module, *resources.Service) string {
	serviceDir, _ := unitDirectory(UnitKindService)
	return func(module *resources.Module, service *resources.Service) string {
		return filepath.Join(root, "modules", module.Name, serviceDir, service.Name)
	}
}

// prepareSnapshotRegistry validates the environment's registry and, when it
// declares an auth method, logs in so `docker push` can resolve the immutable
// snapshot digest. Managed registries authenticate out-of-band and declare no
// auth, so login is skipped. Runs once per render rather than per service.
func prepareSnapshotRegistry(ctx context.Context, env *environments.Environment) error {
	if env.Registry == nil || strings.TrimSpace(env.Registry.URL) == "" {
		return fmt.Errorf("environment %s must declare registry.url for an immutable GitOps snapshot", env.Name)
	}
	builder.SetRepository(env.Registry.URL)
	if env.Registry.Auth != "" {
		if err := builder.RegistryLogin(ctx, env.Registry.URL, env.Registry.Auth); err != nil {
			return fmt.Errorf("authenticate snapshot registry: %w", err)
		}
	}
	return nil
}

func renderServiceFlow(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	service *resources.Service,
	env *environments.Environment,
	standAlone bool,
	validateCluster bool,
	sink orchestration.OutputSink,
	destination func(*resources.Module, *resources.Service) string,
	record func(map[string]*builderv0.DeploymentOutput),
	recordServices func(map[string]*resources.Service),
	recordSelfEndpoints func(map[string]map[string]string),
) (result error) {
	if err := selectionguard.RejectUnboundExecution(workspace.Dir(), module.Dir()); err != nil {
		return err
	}
	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.SnapshotMode)
	if err != nil {
		return err
	}
	flow.WithPush(true)
	if sink != nil {
		flow.WithOutputSink(sink)
	}
	flow.WithStandAlone(standAlone)
	flow.WithClusterValidation(validateCluster)
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
	if recordSelfEndpoints != nil {
		recordSelfEndpoints(flow.SelfEndpoints(ctx))
	}
	if recordServices != nil {
		services := map[string]*resources.Service{}
		for _, unique := range flow.OrderedServiceUniques() {
			loaded, err := flow.ServiceFromUnique(unique)
			if err != nil {
				return err
			}
			services[unique] = loaded
		}
		recordServices(services)
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
