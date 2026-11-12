package gitops

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/google/uuid"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/timings"
)

// CompositionDeploy is every service a composition renders, deployed once.
//
// Rendering a module deploys the dependency closure of each of its root
// services through an agent, and a composition's modules reach one another —
// most of them through the account service and its stores — so rendering the
// modules one after another deployed the same services again for every root of
// every module: the same agent started, the same recipe decided, the same
// registry asked, to arrive at the same files. The closure is a property of
// the composition, not of the module asking: Codefly orders the whole graph
// topologically, and one flow seeded with every root walks that order once.
//
// What the flow produced is held per module, filtered to the services a render
// of that module alone would have deployed, so a module's render reads exactly
// what it read when it deployed them itself. The files each service rendered
// wait in a scratch tree beside the modules' own, and are copied into the
// module's staged tree when its render comes to them.
type CompositionDeploy struct {
	scratch     string
	scratchRoot *renderRoot
	modules     map[string]*moduleDeploy

	indexMu sync.Mutex
	index   *meshComposition
}

// meshIndex is the composition's mesh index, built once for every module's
// render: it is read from the workspace and the environment, which the render
// does not change, and it is only read afterwards.
func (deploy *CompositionDeploy) meshIndex(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (*meshComposition, error) {
	if deploy == nil || env == nil || env.Host == nil {
		return nil, nil
	}
	deploy.indexMu.Lock()
	defer deploy.indexMu.Unlock()
	if deploy.index == nil {
		defer timings.Start("composition.mesh-index")()
		index, err := indexMeshComposition(ctx, workspace, env)
		if err != nil {
			return nil, err
		}
		deploy.index = index
	}
	return deploy.index, nil
}

// moduleDeploy is the part of a composition's deploy one module's render reads.
type moduleDeploy struct {
	// members are the services of the module itself the deploy produced.
	members          []string
	outputs          map[string]*builderv0.DeploymentOutput
	selfEndpoints    map[string]map[string]string
	hostRoutes       map[string]map[string]string
	endpointCarriers map[string]string
	deployed         map[string]*basev0.Configuration
	secretKeys       map[string][]string
	inClusterPorts   map[string]map[string]uint32
}

// DeployComposition deploys, once, every service the given modules render in
// env, writing their trees to a scratch directory that Close removes.
//
// The configuration of every root is checked first, before anything is built or
// pushed, as each module's render used to check its own.
func DeployComposition(
	ctx context.Context,
	workspace *resources.Workspace,
	env *environments.Environment,
	modules []*resources.Module,
	validateCluster, rebuild bool,
	sink orchestration.OutputSink,
) (_ *CompositionDeploy, err error) {
	defer timings.Start("composition.deploy")()
	type moduleRoots struct {
		module  *resources.Module
		roots   []*resources.Service
		uniques []string
	}
	var plan []moduleRoots
	var allRoots []*resources.Service
	var origin *moduleRoots
	// Everything a module's render refused before it built an image is refused
	// for every module before the first image is built.
	if err = preflightModules(ctx, workspace, env, modules); err != nil {
		return nil, err
	}
	// A module with nothing to deploy still renders (its bootstrap or bundle),
	// reading nothing from the deploy.
	held := make(map[string]*moduleDeploy, len(modules))
	for _, module := range modules {
		held[module.Name] = &moduleDeploy{}
	}
	for _, module := range modules {
		services, loadErr := loadModuleServices(ctx, module, env)
		if loadErr != nil {
			return nil, loadErr
		}
		roots, rootErr := moduleRenderRoots(module.Name, services)
		if rootErr != nil {
			return nil, rootErr
		}
		if len(roots) == 0 {
			continue
		}
		entry := moduleRoots{module: module, roots: roots}
		for _, root := range roots {
			entry.uniques = append(entry.uniques, resources.ServiceUnique(module.Name, root.Name))
		}
		plan = append(plan, entry)
		allRoots = append(allRoots, roots...)
	}
	if len(plan) == 0 {
		return &CompositionDeploy{modules: held}, nil
	}
	origin = &plan[0]
	if err = orchestration.PlanConfigurationReferences(ctx, workspace, env, allRoots, false); err != nil {
		return nil, err
	}
	if err = prepareSnapshotRegistry(ctx, env); err != nil {
		return nil, err
	}

	parent := filepath.Dir(moduleRenderDestination(workspace, origin.module.Name))
	if err = os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create the composition deploy parent: %w", err)
	}
	scratch := filepath.Join(parent, ".codefly-composition-"+uuid.NewString())
	if err = os.Mkdir(scratch, 0o755); err != nil {
		return nil, err
	}
	deploy := &CompositionDeploy{scratch: scratch, modules: held}
	defer func() {
		if err != nil {
			_ = deploy.Close()
		}
	}()
	if deploy.scratchRoot, err = newRenderRoot(scratch); err != nil {
		return nil, err
	}

	serviceDir, _ := unitDirectory(UnitKindService)
	destination := func(rendered *resources.Module, service *resources.Service) string {
		owner := ""
		if rendered != nil {
			owner = rendered.Name
		}
		return filepath.Join(deploy.scratchRoot.path, owner, serviceDir, service.Name)
	}
	var coRoots []string
	for _, entry := range plan {
		coRoots = append(coRoots, entry.uniques...)
	}
	coRoots = coRoots[1:]
	flow, err := deploySnapshot(ctx, workspace, origin.module, origin.roots[0], coRoots, env,
		renderBuild{validateCluster: validateCluster, rebuild: rebuild}, sink, destination)
	if err != nil {
		return nil, fmt.Errorf("deploy the composition: %w", err)
	}
	defer func() {
		if stopErr := flow.Stop(); err == nil && stopErr != nil {
			err = stopErr
		}
	}()

	outputs := flow.DeploymentOutputs()
	selfEndpoints := flow.SelfEndpoints(ctx)
	hostRoutes, err := flow.HostRouteCarriers(ctx)
	if err != nil {
		return nil, err
	}
	deployed, secretKeys := flow.DeployedConfigurations(), flow.DeployedSecretKeys()
	inClusterPorts := flow.InClusterPorts(ctx)
	for _, entry := range plan {
		closure, closureErr := flow.SnapshotClosure(ctx, entry.uniques...)
		if closureErr != nil {
			return nil, closureErr
		}
		carriers, carrierErr := flow.EndpointCarriers(ctx, entry.module.Name, resources.NewContainerNetworkAccess())
		if carrierErr != nil {
			return nil, carrierErr
		}
		share := deploy.modules[entry.module.Name]
		share.outputs, share.selfEndpoints, share.hostRoutes = within(outputs, closure), within(selfEndpoints, closure), within(hostRoutes, closure)
		share.endpointCarriers = carriers
		share.deployed, share.secretKeys = within(deployed, closure), within(secretKeys, closure)
		share.inClusterPorts = within(inClusterPorts, closure)
		prefix := entry.module.Name + "/"
		for _, unique := range closure {
			if len(unique) > len(prefix) && unique[:len(prefix)] == prefix {
				share.members = append(share.members, unique[len(prefix):])
			}
		}
	}
	return deploy, nil
}

// preflightModules holds every module to the refusals its own render makes
// before it builds anything, so a refusal costs no build: the selection guard,
// the authored render configurations, the host's delivery endpoint, the owned
// path, the module's package and its contract catalog.
func preflightModules(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, modules []*resources.Module) error {
	defer timings.Start("composition.preflight")()
	if err := refuseAuthoredRenderConfigurations(ctx, workspace, env.Runtime()); err != nil {
		return err
	}
	if _, err := resolveDeliveryTarget(ctx, workspace, env); err != nil {
		return err
	}
	for _, module := range modules {
		if err := selectionguard.RejectUnboundExecution(workspace.Dir(), module.Dir()); err != nil {
			return err
		}
		if _, err := moduleOwnedPath(workspace, env, module.Name); err != nil {
			return err
		}
		if _, _, err := modulePackage(module.Dir()); err != nil {
			return err
		}
		if _, err := loadContractCatalog(module.Dir()); err != nil {
			return err
		}
	}
	return nil
}

// within is the entries of values keyed by a member of closure.
func within[V any](values map[string]V, closure []string) map[string]V {
	kept := make(map[string]V, len(closure))
	for unique, value := range values {
		if slices.Contains(closure, unique) {
			kept[unique] = value
		}
	}
	return kept
}

// Close removes the scratch tree.
func (deploy *CompositionDeploy) Close() error {
	if deploy == nil || deploy.scratch == "" {
		return nil
	}
	if deploy.scratchRoot != nil {
		_ = deploy.scratchRoot.Close()
	}
	return os.RemoveAll(deploy.scratch)
}

// stageInto fills a module's staged render with what its deploy produced: the
// trees of its own services copied under stage, and what the deploy learned
// about them and about the services they reach read into rendered.
func (deploy *CompositionDeploy) stageInto(module, stage string, tree *renderRoot, rendered *renderedRoots) error {
	held, ok := deploy.modules[module]
	if !ok {
		return fmt.Errorf("the composition deploy holds nothing for module %s", module)
	}
	serviceDir, _ := unitDirectory(UnitKindService)
	for _, name := range held.members {
		source := filepath.Join(deploy.scratchRoot.path, module, serviceDir, name)
		if _, err := deploy.scratchRoot.stat(filepath.Dir(source), name); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := tree.copyTree(deploy.scratchRoot, source, filepath.Join(stage, serviceDir, name), true); err != nil {
			return fmt.Errorf("stage %s/%s: %w", module, name, err)
		}
	}
	maps.Copy(rendered.outputs, held.outputs)
	maps.Copy(rendered.selfEndpoints, held.selfEndpoints)
	maps.Copy(rendered.hostRoutes, held.hostRoutes)
	maps.Copy(rendered.deployed, held.deployed)
	maps.Copy(rendered.secretKeys, held.secretKeys)
	maps.Copy(rendered.inClusterPorts, held.inClusterPorts)
	maps.Copy(rendered.endpointCarriers, held.endpointCarriers)
	return nil
}
