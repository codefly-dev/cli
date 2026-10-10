package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	coreservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
)

func projectServiceConfiguration(ctx context.Context, root string, service *resources.Service, env *environments.Environment, scope unitScope, injection serviceInjection) error {
	if err := projectConfigurationValues(ctx, root, service.Name, env); err != nil {
		return fmt.Errorf("project service %s configuration: %w", service.Name, err)
	}
	// The public origin of this service's exposed endpoints, from the
	// environment's declarations alone. An origin that names the machine the
	// workload runs on refuses here, before anything is written; an endpoint the
	// environment fixes no origin for is refused for the whole composition where
	// the carriers are derived.
	injection, err := withPublicOriginCarriers(scope.Module, service, env, injection)
	if err != nil {
		return err
	}
	// Before the secret projection: the derived secretKeyRefs are part of what
	// the ExternalSecret must materialize.
	if err := projectServiceInjection(ctx, root, service.Name, env, injection); err != nil {
		return fmt.Errorf("project service %s derived configuration: %w", service.Name, err)
	}
	if _, err := projectServiceSecrets(root, scope, service.Name, env.Name, env.ServiceSecrets); err != nil {
		return fmt.Errorf("project service %s secrets: %w", service.Name, err)
	}
	if _, err := projectServiceAutoscale(root, service.Name, env.Name, scope.Namespace, service.Autoscale); err != nil {
		return fmt.Errorf("project service %s autoscale: %w", service.Name, err)
	}
	if err := projectManagedIdentity(ctx, root, service, env, scope); err != nil {
		return fmt.Errorf("project service %s managed identity: %w", service.Name, err)
	}
	if err := validateProjectedConfiguration(root, service, env, scope); err != nil {
		return err
	}
	if err := validateProjectedInjection(root, service.Name, env, injection); err != nil {
		return err
	}
	if !env.Deployed() {
		return nil
	}
	// Last, and unconditionally: the identity guarantee is about what the cluster
	// receives, so it is checked after every projection and every validation that
	// could have a more specific thing to say about the same overlay.
	return requireOwnServiceAccount(root, env.Name, scope.Module, service.Name)
}

// projectManagedIdentity binds the service's pods to a service account of their
// own, carrying the declared runtime identity when the environment declares one.
// Endpoint addresses and container choices remain owned by their existing
// renderers.
//
// Every deployed workload runs under an account of its own, declared identity or
// not, because the account is what anything downstream has to name to tell two
// workloads apart: an authorization rule, a network identity and a cloud binding
// all name a principal, and a principal shared by several workloads grants each
// of them whatever any one of them was granted. An account with no annotations
// carries no privilege by itself; it only makes the workload nameable.
//
// The projection writes the account the service does not already have, and
// requireOwnServiceAccount then holds the EFFECTIVE render to the rule —
// whatever the file layout underneath it, and whatever an overlay does after
// this ran. Writing and checking are separate on purpose: the write is a
// convenience, the check is the guarantee, and a shape the write cannot reach
// (an account bound only by a patch, a workload in a file no base scan sees) is
// then refused rather than missed.
//
// A local cluster keeps the previous behaviour: these accounts exist to be named
// in rules someone else writes, and a developer's own cluster has no such rules,
// so a render there is left byte-for-byte as it was.
func projectManagedIdentity(
	ctx context.Context,
	serviceRoot string,
	service *resources.Service,
	env *environments.Environment,
	scope unitScope,
) error {
	if err := env.Validate(); err != nil {
		return err
	}
	identity, err := soleWorkloadIdentity(scope.Module, service.Name, consumedManagedServices(service, scope.Module, env), env)
	if err != nil {
		return err
	}
	if identity == nil && !env.Deployed() {
		return nil
	}
	base := filepath.Join(serviceRoot, "base")
	if env.Deployed() {
		state, err := deployedAccountState(serviceRoot, base, env.Name)
		if err != nil {
			return err
		}
		switch state {
		case accountIsSomeoneElses:
			// Writing cannot fix an identity that is already someone else's, and
			// taking the name already bound is how two services end up sharing one
			// principal. The render leaves the tree alone; requireOwnServiceAccount
			// is what refuses it, after the configuration checks have had their say
			// about the same overlay.
			return nil
		case accountHasNoWorkload:
			// Nothing to tell apart from another's. A declared identity is
			// different — it names a principal the cell has already bound — so
			// that case falls through to core's own refusal.
			if identity == nil {
				return nil
			}
		case accountIsItsOwn:
			// Already nameable. Only a declared identity's annotations are still
			// worth writing.
			if identity == nil {
				return nil
			}
		case accountIsUnbound:
		}
	}
	account := &coreservices.WorkloadServiceAccount{}
	var podLabels map[string]string
	if identity != nil {
		account.Annotations = identity.Annotations
		podLabels = identity.Labels
	}
	overlay := &coreservices.PodTemplateOverlay{}
	if err := overlay.AttachServiceAccount(account, podLabels); err != nil {
		return err
	}
	return coreservices.ProjectServiceAccount(ctx, base, scope.Namespace, service.Name, overlay)
}

// managedConsumption is one managed service a workload dials, named by the
// module-qualified identity of the service it replaces so an error can tell two
// same-named dependencies apart.
type managedConsumption struct {
	unique  string
	managed environments.EnvironmentManagedService
}

// consumedManagedServices returns, in a stable order, the environment's managed
// services this service's pods dial. It walks the service's own dependencies
// rather than the environment's entries: a dependency carries the module it
// resolves in, which is what decides whether this environment manages it, while
// an entry keyed by a bare name alone cannot say which module's service it
// replaced. module is the module the consuming service renders in, the default
// for a dependency that names no module of its own.
//
// Only edges that constrain running count: a build or schema edge on a database
// is read by the toolchain that generates code, not by the workload, so stamping
// its identity onto the pod would authenticate a container that never opens the
// connection.
func consumedManagedServices(service *resources.Service, module string, env *environments.Environment) []managedConsumption {
	var consumed []managedConsumption
	for _, dependency := range service.ServiceDependencies {
		if !dependency.Kind.Participates(resources.StageRun) {
			continue
		}
		dependencyModule := dependency.Module
		if dependencyModule == "" {
			dependencyModule = module
		}
		managed, replaced := env.ManagedService(dependencyModule, dependency.Name)
		if !replaced {
			continue
		}
		consumed = append(consumed, managedConsumption{
			unique:  resources.ServiceUnique(dependencyModule, dependency.Name),
			managed: managed,
		})
	}
	sort.Slice(consumed, func(i, j int) bool { return consumed[i].unique < consumed[j].unique })
	return consumed
}

// soleWorkloadIdentity combines a service's own identity with those its managed
// dependencies declare. A pod runs under a single ServiceAccount, so two
// endpoints naming different principals have no rendering: whichever was stamped
// last would win and the other endpoint would refuse the workload at runtime
// with nothing in the deploy to show for it. Several endpoints reached as the
// same principal are one identity and render as one.
func soleWorkloadIdentity(module, service string, consumed []managedConsumption, env *environments.Environment) (*environments.EnvironmentWorkloadIdentity, error) {
	identity := env.WorkloadIdentityFor(module, service)
	var declaring []string
	for _, consumption := range consumed {
		declared := consumption.managed.Identity
		if declared == nil {
			continue
		}
		if identity != nil && !reflect.DeepEqual(identity, declared) {
			return nil, fmt.Errorf("service %q consumes managed services %s, which declare different runtime identities; a pod authenticates as one",
				service, strings.Join(append(declaring, consumption.unique), ", "))
		}
		identity = declared
		declaring = append(declaring, consumption.unique)
	}
	return identity, nil
}

// projectRenderedServiceConfiguration projects every service tree a flow
// rendered under stage/modules/<module>/services/<service>. Each is scoped to
// its own module: a dependency the flow loaded from another module binds to that
// module's namespace, the one its addresses were synthesized in.
func projectRenderedServiceConfiguration(
	ctx context.Context,
	stage string,
	workspace *resources.Workspace,
	env *environments.Environment,
	graph map[string]*resources.Service,
	injections renderInjections,
	templates renderTemplates,
) error {
	modulesRoot := filepath.Join(stage, "modules")
	moduleEntries, err := os.ReadDir(modulesRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Two modules rendering into one namespace is the identity question a single
	// service cannot answer, and this is the only place that sees every service
	// of the render.
	claimed := namespaceAccounts{}
	for _, moduleEntry := range moduleEntries {
		if !moduleEntry.IsDir() {
			continue
		}
		servicesRoot := filepath.Join(modulesRoot, moduleEntry.Name(), serviceUnitDir)
		serviceEntries, err := os.ReadDir(servicesRoot)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, serviceEntry := range serviceEntries {
			if !serviceEntry.IsDir() {
				continue
			}
			// A directory entry's name is one path component. Joining it as read
			// would let a name the filesystem returned decide where the render
			// looks, so it is reduced to its base here, once, and the unit path
			// below cannot leave the tree it was listed from.
			serviceName := filepath.Base(serviceEntry.Name())
			service := graph[resources.ServiceUnique(moduleEntry.Name(), serviceEntry.Name())]
			if service == nil {
				continue
			}
			scope := moduleScope(env, workspace, moduleEntry.Name())
			scope.Templates = templates
			if err := projectServiceConfiguration(
				ctx,
				filepath.Join(servicesRoot, serviceName),
				service,
				env,
				scope,
				injections.forService(moduleEntry.Name(), serviceEntry.Name()),
			); err != nil {

				return fmt.Errorf("project service %s configuration: %w", serviceEntry.Name(), err)
			}
			if claimErr := claimed.recordUnit(
				filepath.Join(servicesRoot, serviceName),
				env,
				resources.ServiceUnique(moduleEntry.Name(), serviceEntry.Name()),
			); claimErr != nil {
				return claimErr
			}
		}
	}
	return nil
}
