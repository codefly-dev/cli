package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	modulecontract "github.com/codefly-dev/core/contracts/module"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

// renderBuild is what a render decides about the image builds its services run:
// how much of the graph to drive, whether to validate against a cluster, and
// whether to rebuild an image whose inputs have not changed.
type renderBuild struct {
	standAlone      bool
	validateCluster bool
	rebuild         bool
}

func RenderModule(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *environments.Environment, project string, sink orchestration.OutputSink) (RenderResult, error) {
	return renderModuleTree(ctx, workspace, module, env, project, sink, true, renderBuild{})
}

func renderModuleTree(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	env *environments.Environment,
	project string,
	sink orchestration.OutputSink,
	includeBootstrap bool,
	build renderBuild,
) (RenderResult, error) {
	if err := selectionguard.RejectUnboundExecution(workspace.Dir(), module.Dir()); err != nil {
		return RenderResult{}, err
	}
	if err := environments.ValidateWorkspace(ctx, workspace); err != nil {
		return RenderResult{}, err
	}
	ownedPath, err := moduleOwnedPath(workspace, env, module.Name)
	if err != nil {
		return RenderResult{}, err
	}
	// The module's namespace, not the environment's: a workspace composing
	// several modules gives each its own, and the render record, the Argo
	// destinations derived from it, the quota and the secret projections all
	// take it from here.
	scope := moduleScope(env, workspace, module.Name)
	// The host's delivery API is resolved before anything renders: a host block
	// naming an endpoint the composition does not have is refused here, not
	// after images are built.
	deliveryTarget, err := resolveDeliveryTarget(ctx, workspace, env)
	if err != nil {
		return RenderResult{}, err
	}
	options := &RenderOptions{
		Destination:    moduleRenderDestination(workspace, module.Name),
		Module:         module.Name,
		Environment:    env.Name,
		Namespace:      scope.Namespace,
		AppProject:     project,
		Promotable:     true,
		OwnedPath:      ownedPath,
		Workspace:      workspace.Name,
		Host:           env.Host,
		Target:         env,
		DeliveryTarget: deliveryTarget,
	}
	render := &moduleRender{
		workspace: workspace, module: module, env: env, scope: &scope,
		sink: sink, includeBootstrap: includeBootstrap, build: build, options: options,
	}
	result, err := RenderOwnedTree(ctx, options, render.stage)
	if err != nil {
		return RenderResult{}, err
	}
	result.StaleGroupConsumers = render.staleConsumers
	return result, nil
}

// moduleOwnedPath is where the module's tree lives in the delivery
// repository: under the environment's gitops path when it declares one, the
// workspace's default otherwise.
func moduleOwnedPath(workspace *resources.Workspace, env *environments.Environment, module string) (string, error) {
	ownedPath := filepath.ToSlash(filepath.Join("deployments", "modules", module))
	gitopsPath := ""
	if env.Gitops != nil {
		gitopsPath = env.Gitops.Path
	} else if defaults, err := environments.WorkspaceGitops(workspace); err != nil {
		return "", err
	} else if defaults != nil {
		gitopsPath = defaults.Path
	}
	if gitopsPath != "" {
		ownedPath = filepath.ToSlash(filepath.Join(gitopsPath, ownedPath))
	}
	return ownedPath, nil
}

// moduleRender is one module's render in progress: what it was asked to
// render, and what staging it learned that the result reports.
type moduleRender struct {
	workspace        *resources.Workspace
	module           *resources.Module
	env              *environments.Environment
	scope            *unitScope
	sink             orchestration.OutputSink
	includeBootstrap bool
	build            renderBuild
	options          *RenderOptions
	// staleConsumers are the sibling group consumers the render found stale.
	staleConsumers map[string][]string
}

// stage fills the staged owned tree: every root service rendered through its
// agent, the units inventoried, the module's presence and authority declared,
// and the module bundle or the environment bootstrap staged beside them.
func (r *moduleRender) stage(ctx context.Context, stage string) error {
	services, err := loadModuleServices(ctx, r.module)
	if err != nil {
		return err
	}
	// The workspace configuration groups this render bakes in, digested.
	// Publish holds this tree and its sibling consumers to them; the render
	// only reports the siblings that are stale now, so they are rendered
	// next rather than found at publish.
	digests, err := currentGroupDigests(ctx, r.workspace, r.env, r.module)
	if err != nil {
		return err
	}
	r.staleConsumers, err = staleGroupConsumers(r.workspace.Dir(), r.module.Name, r.env.Name, digests)
	if err != nil {
		return err
	}
	r.options.WorkspaceConfigurationDigests = digests
	catalog, roots, err := r.resolveInputs(ctx, services)
	if err != nil {
		return err
	}
	rendered, err := r.renderRoots(ctx, roots, stage)
	if err != nil {
		return err
	}
	r.scope.Templates = renderTemplates{}
	if err = collectRenderTemplates(r.scope.Templates, rendered.deployed, rendered.secretKeys); err != nil {
		return err
	}
	injections, err := deriveRenderInjections(ctx, r.workspace, rendered.selfEndpoints, r.sink)
	if err != nil {
		return err
	}
	if err = r.inventoryUnits(ctx, services, catalog, stage, rendered, injections); err != nil {
		return err
	}
	if err = r.declareInstances(ctx, services); err != nil {
		return err
	}
	if r.module.Agent != nil {
		modulePath := filepath.Join(stage, moduleBundleDir)
		if err = renderModuleBundle(ctx, r.workspace, r.module, r.env, modulePath, r.options.Units); err != nil {
			return err
		}
		r.options.ModulePath = moduleBundleDir
		r.options.ModuleIncludesUnits = moduleIncludesUnits(modulePath, r.env.Name)
		return nil
	}
	if !r.includeBootstrap {
		return nil
	}
	return r.stageBootstrap(stage)
}

// loadModuleServices loads every service the module references.
func loadModuleServices(ctx context.Context, module *resources.Module) ([]*resources.Service, error) {
	services := make([]*resources.Service, 0, len(module.ServiceReferences))
	for _, reference := range module.ServiceReferences {
		service, err := module.LoadServiceFromName(ctx, reference.Name)
		if err != nil {
			return nil, fmt.Errorf("load service %s: %w", reference.Name, err)
		}
		services = append(services, service)
	}
	return services, nil
}

// resolveInputs resolves what the render reads before any image is built:
// the module's package, its contract catalog and its render roots, with every
// configuration reference of every root's graph checked, and the snapshot
// registry prepared when there is a service to build.
func (r *moduleRender) resolveInputs(ctx context.Context, services []*resources.Service) (*contractCatalogDocument, []*resources.Service, error) {
	pkg, err := modulePackage(r.module.Dir())
	if err != nil {
		return nil, nil, err
	}
	r.options.Package = pkg
	catalog, err := loadContractCatalog(r.module.Dir())
	if err != nil {
		return nil, nil, err
	}
	roots, err := moduleRenderRoots(r.module.Name, services)
	if err != nil {
		return nil, nil, err
	}
	// A configuration error refuses the render before any image is built or
	// pushed, listing every unresolved reference of every root's graph.
	if err = orchestration.PlanConfigurationReferences(ctx, r.workspace, r.env, roots, false); err != nil {
		return nil, nil, err
	}
	// The registry is only needed to build and push service images. A module
	// with no buildable services still renders its bootstrap kustomize tree or
	// module bundle, which need no registry, so demand one only when there is a
	// service to build.
	if len(roots) > 0 {
		if err = prepareSnapshotRegistry(ctx, r.env); err != nil {
			return nil, nil, err
		}
	}
	return catalog, roots, nil
}

// renderedRoots is what rendering the root services through their agents
// learned: each one's deployment output, self endpoints, deployed
// configuration with its declared secret keys, and in-cluster ports.
type renderedRoots struct {
	outputs        map[string]*builderv0.DeploymentOutput
	selfEndpoints  map[string]map[string]string
	deployed       map[string]*basev0.Configuration
	secretKeys     map[string][]string
	inClusterPorts map[string]map[string]uint32
}

// renderRoots renders every root service through its agent into the stage.
func (r *moduleRender) renderRoots(ctx context.Context, roots []*resources.Service, stage string) (*renderedRoots, error) {
	rendered := &renderedRoots{
		outputs:        make(map[string]*builderv0.DeploymentOutput),
		selfEndpoints:  make(map[string]map[string]string),
		deployed:       make(map[string]*basev0.Configuration),
		secretKeys:     make(map[string][]string),
		inClusterPorts: make(map[string]map[string]uint32),
	}
	destinations := moduleStageDestinations(r.workspace, r.module, stage)
	for _, service := range roots {
		if flowErr := serviceFlow(
			ctx,
			r.workspace,
			r.module,
			service,
			r.env,
			renderBuild{validateCluster: r.build.validateCluster, rebuild: r.build.rebuild},
			r.sink,
			destinations,
			func(outputs map[string]*builderv0.DeploymentOutput) {
				for unique, output := range outputs {
					rendered.outputs[unique] = output
				}
			},
			nil,
			func(endpoints map[string]map[string]string) {
				for unique, variables := range endpoints {
					rendered.selfEndpoints[unique] = variables
				}
			},
			func(configurations map[string]*basev0.Configuration, keys map[string][]string) {
				for unique, configuration := range configurations {
					rendered.deployed[unique] = configuration
				}
				for unique, declared := range keys {
					rendered.secretKeys[unique] = declared
				}
			},
			func(ports map[string]map[string]uint32) {
				for unique, declared := range ports {
					rendered.inClusterPorts[unique] = declared
				}
			},
		); flowErr != nil {
			return nil, fmt.Errorf("render service %s: %w", service.Name, flowErr)
		}
	}
	return rendered, nil
}

// inventoryUnits records every service of the module as a unit of the
// render: a managed service by the bootstrap bundle it retains, if any; an
// ordinary one by the tree its agent rendered, with its configuration
// projected and its declared endpoint ports verified against the cluster
// ports the render learned.
func (r *moduleRender) inventoryUnits(
	ctx context.Context,
	services []*resources.Service,
	catalog *contractCatalogDocument,
	stage string,
	rendered *renderedRoots,
	injections renderInjections,
) error {
	unitDir, _ := unitDirectory(UnitKindService)
	for _, service := range services {
		managedService, managed := r.env.ManagedService(r.module.Name, service.Name)
		entry := InventoryUnit{
			Kind:      UnitKindService,
			Module:    r.module.Name,
			Name:      service.Name,
			Managed:   managed,
			Contracts: catalog.exposedContracts(r.module.Name, service.Name),
		}
		unique := resources.ServiceUnique(r.module.Name, service.Name)
		if managed {
			bootstrap, bundleErr := retainManagedBundle(
				filepath.Join(stage, unitDir, service.Name),
				service.Name,
				r.env.Name,
				r.scope.Namespace,
				managedService.SecretReferences,
			)
			if bundleErr != nil {
				return fmt.Errorf("select managed service %s bundle: %w", service.Name, bundleErr)
			}
			if bootstrap {
				entry.Path = filepath.ToSlash(filepath.Join(unitDir, service.Name))
				entry.Bootstrap = true
				entry.Output = inventoryKubernetesOutput(rendered.outputs[unique])
			}
		} else {
			entry.Path = filepath.ToSlash(filepath.Join(unitDir, service.Name))
			entry.Output = inventoryKubernetesOutput(rendered.outputs[unique])
			if entry.Output == nil {
				return fmt.Errorf("service %s returned no Kubernetes deployment evidence", service.Name)
			}
			if projectErr := projectServiceConfiguration(
				ctx,
				filepath.Join(stage, unitDir, service.Name),
				service,
				r.env,
				*r.scope,
				injections.forService(r.module.Name, service.Name),
			); projectErr != nil {
				return projectErr
			}
			if portsErr := verifyDeclaredEndpointPorts(
				filepath.Join(stage, unitDir, service.Name),
				r.env.Name,
				r.module.Name,
				service,
				rendered.inClusterPorts[unique],
			); portsErr != nil {
				return portsErr
			}
		}
		r.options.Units = append(r.options.Units, entry)
	}
	sort.Slice(r.options.Units, func(i, j int) bool {
		return r.options.Units[i].Name < r.options.Units[j].Name
	})
	return nil
}

// declareInstances declares presence from the resolution this render already
// holds — the module it is rendering, the services it loaded from it, the
// package it resolved, and the units it just assembled — so the binding
// cannot describe a composition the workloads did not come from. Authority is
// effective from a presence generation, so a module declaring no presence
// asks for none.
func (r *moduleRender) declareInstances(ctx context.Context, services []*resources.Service) error {
	instance, undeclared, err := presenceInstanceOf(r.module, services, r.env, r.options)
	if err != nil {
		return err
	}
	r.options.UndeclaredPresence = undeclared
	if instance == nil {
		// A module that publishes a contract asks for authority, and
		// authority is effective from a presence generation: with no
		// presence to declare, the request would be dropped silently.
		if _, loadErr := modulecontract.Load(r.module.Dir()); loadErr == nil {
			return fmt.Errorf("module %s publishes %s but declares no presence (%s); authority is effective from a presence generation, so a module with a contract must declare presence", r.module.Name, modulecontract.FileName, undeclared)
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
		r.options.UndeclaredAuthority = "the module declares no presence, so no authority can be effective from it"
		return nil
	}
	r.options.SolutionInstances = []SolutionInstance{*instance}
	r.options.AuthorityInstances, r.options.UndeclaredAuthority, err = authorityInstancesOf(ctx, r.workspace, r.module, services, r.env, instance.Units)
	return err
}

// stageBootstrap copies the module's environment bootstrap into the stage and
// projects the namespace's resource quota into it; a module with no kustomize
// tree has none to stage.
func (r *moduleRender) stageBootstrap(stage string) error {
	static := filepath.Join(r.module.Dir(), "deployment", "kustomize")
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
	if err = copyEnvironmentBootstrap(static, r.env.Name, bootstrap); err != nil {
		return fmt.Errorf("copy module environment bootstrap: %w", err)
	}
	if _, err = projectResourceQuota(bootstrap, r.env.Name, r.scope.Namespace, r.env.ResourceQuota); err != nil {
		return fmt.Errorf("project module namespace resource quota: %w", err)
	}
	return nil
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
	return renderService(ctx, workspace, module, service, env, project, renderBuild{standAlone: standAlone}, sink)
}

func renderService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *environments.Environment, project string, build renderBuild, sink orchestration.OutputSink) (RenderResult, error) {
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
		// A configuration error refuses the render before any image is built or
		// pushed.
		if err := orchestration.PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{service}, build.standAlone); err != nil {
			return err
		}
		if err := prepareSnapshotRegistry(ctx, env); err != nil {
			return err
		}
		var graph map[string]*resources.Service
		var selfEndpoints map[string]map[string]string
		var deployed map[string]*basev0.Configuration
		var secretKeys map[string][]string
		var inClusterPorts map[string]map[string]uint32
		if err := serviceFlow(
			ctx,
			workspace,
			module,
			service,
			env,
			build,
			sink,
			serviceRenderDestinations(stage),
			nil,
			func(services map[string]*resources.Service) { graph = services },
			func(rendered map[string]map[string]string) { selfEndpoints = rendered },
			func(rendered map[string]*basev0.Configuration, keys map[string][]string) {
				deployed, secretKeys = rendered, keys
			},
			func(rendered map[string]map[string]uint32) { inClusterPorts = rendered },
		); err != nil {
			return err
		}
		injections, err := deriveRenderInjections(ctx, workspace, selfEndpoints, sink)
		if err != nil {
			return err
		}
		templates := renderTemplates{}
		if err := collectRenderTemplates(templates, deployed, secretKeys); err != nil {
			return err
		}
		if err := projectRenderedServiceConfiguration(ctx, stage, workspace, env, graph, injections, templates); err != nil {
			return err
		}
		return verifyGraphDeclaredEndpointPorts(stage, env, graph, inClusterPorts)

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

// serviceFlow renders one root service's dependency graph through its service
// agents. It is a variable so a test can stand an in-process agent in for the
// flow, the way stubBuild stands in for the image build boundary.
var serviceFlow = renderServiceFlow

func renderServiceFlow(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	service *resources.Service,
	env *environments.Environment,
	build renderBuild,
	sink orchestration.OutputSink,
	destination func(*resources.Module, *resources.Service) string,
	record func(map[string]*builderv0.DeploymentOutput),
	recordServices func(map[string]*resources.Service),
	recordSelfEndpoints func(map[string]map[string]string),
	recordConfigurations func(map[string]*basev0.Configuration, map[string][]string),
	recordInClusterPorts func(map[string]map[string]uint32),
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
	flow.WithStandAlone(build.standAlone)
	flow.WithClusterValidation(build.validateCluster)
	flow.WithRebuild(build.rebuild)
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
	if err := flow.Deploy(ctx); err != nil {
		return err
	}
	if record != nil {
		record(flow.DeploymentOutputs())
	}
	if recordSelfEndpoints != nil {
		recordSelfEndpoints(flow.SelfEndpoints(ctx))
	}
	if recordConfigurations != nil {
		recordConfigurations(flow.DeployedConfigurations(), flow.DeployedSecretKeys())
	}
	if recordInClusterPorts != nil {
		recordInClusterPorts(flow.InClusterPorts(ctx))
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
