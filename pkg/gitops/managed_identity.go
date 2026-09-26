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
	if err := projectManagedIdentity(ctx, root, service, env, scope.Namespace); err != nil {
		return fmt.Errorf("project service %s managed identity: %w", service.Name, err)
	}
	if err := validateProjectedConfiguration(root, service, env, scope); err != nil {
		return err
	}
	return validateProjectedInjection(root, service.Name, env, injection)
}

// projectManagedIdentity applies only the declared runtime identity, into the
// namespace the service's manifests bind to. Endpoint addresses and container
// choices remain owned by their existing renderers.
func projectManagedIdentity(
	ctx context.Context,
	serviceRoot string,
	service *resources.Service,
	env *environments.Environment,
	namespace string,
) error {
	if err := env.Validate(); err != nil {
		return err
	}
	consumed := consumedManagedServices(service, env)
	identity, err := soleWorkloadIdentity(service.Name, consumed, env)
	if err != nil {
		return err
	}
	if identity == nil {
		return nil
	}
	overlay := &coreservices.PodTemplateOverlay{}
	if err := overlay.AttachServiceAccount(&coreservices.WorkloadServiceAccount{Annotations: identity.Annotations}, identity.Labels); err != nil {
		return err
	}
	return coreservices.ProjectServiceAccount(ctx, filepath.Join(serviceRoot, "base"), namespace, service.Name, overlay)
}

// consumedManagedServices returns, in a stable order, the environment's managed
// services this service's pods dial. Only edges that constrain running count: a
// build or schema edge on a database is read by the toolchain that generates
// code, not by the workload, so stamping its identity onto the pod would
// authenticate a container that never opens the connection.
func consumedManagedServices(service *resources.Service, env *environments.Environment) []string {
	var consumed []string
	for name := range env.ManagedServices {
		dependency := managedDependency(service, name)
		if dependency == nil || !dependency.Kind.Participates(resources.StageRun) {
			continue
		}
		consumed = append(consumed, name)
	}
	sort.Strings(consumed)
	return consumed
}

func managedDependency(service *resources.Service, managed string) *resources.ServiceDependency {
	for _, dependency := range service.ServiceDependencies {
		if dependency.Name == managed {
			return dependency
		}
	}
	return nil
}

// soleWorkloadIdentity combines a service's own identity with those its managed
// dependencies declare. A pod runs under a single ServiceAccount, so two
// endpoints naming different principals have no rendering: whichever was stamped
// last would win and the other endpoint would refuse the workload at runtime
// with nothing in the deploy to show for it. Several endpoints reached as the
// same principal are one identity and render as one.
func soleWorkloadIdentity(service string, consumed []string, env *environments.Environment) (*environments.EnvironmentWorkloadIdentity, error) {
	identity := env.WorkloadIdentity(service)
	var declaring []string
	for _, name := range consumed {
		declared := env.ManagedServices[name].Identity
		if declared == nil {
			continue
		}
		if identity != nil && !reflect.DeepEqual(identity, declared) {
			return nil, fmt.Errorf("service %q consumes managed services %s, which declare different runtime identities; a pod authenticates as one",
				service, strings.Join(append(declaring, name), ", "))
		}
		identity = declared
		declaring = append(declaring, name)
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
			service := graph[resources.ServiceUnique(moduleEntry.Name(), serviceEntry.Name())]
			if service == nil {
				continue
			}
			scope := moduleScope(env, workspace, moduleEntry.Name())
			scope.Templates = templates
			if err := projectServiceConfiguration(
				ctx,
				filepath.Join(servicesRoot, serviceEntry.Name()),
				service,
				env,
				scope,
				injections.forService(moduleEntry.Name(), serviceEntry.Name()),
			); err != nil {

				return fmt.Errorf("project service %s configuration: %w", serviceEntry.Name(), err)
			}
		}
	}
	return nil
}
