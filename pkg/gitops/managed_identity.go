package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	coreservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
)

func projectServiceConfiguration(ctx context.Context, root string, service *resources.Service, env *resources.Environment) error {
	if _, err := projectServiceSecrets(root, service.Name, env.Name, env.Namespace, env.ServiceSecrets); err != nil {
		return fmt.Errorf("project service %s secrets: %w", service.Name, err)
	}
	if _, err := projectServiceAutoscale(root, service.Name, env.Name, env.Namespace, service.Autoscale); err != nil {
		return fmt.Errorf("project service %s autoscale: %w", service.Name, err)
	}
	if err := projectManagedIdentity(ctx, root, service, env); err != nil {
		return fmt.Errorf("project service %s managed identity: %w", service.Name, err)
	}
	return nil
}

// projectManagedIdentity applies only the declared runtime identity. Endpoint
// addresses and container choices remain owned by their existing renderers.
func projectManagedIdentity(
	ctx context.Context,
	serviceRoot string,
	service *resources.Service,
	env *resources.Environment,
) error {
	consumed := consumedManagedServices(service, env)
	if len(consumed) == 0 {
		return nil
	}
	identity, err := soleWorkloadIdentity(service.Name, consumed, env)
	if err != nil {
		return err
	}
	return coreservices.ProjectWorkloadIdentity(ctx, filepath.Join(serviceRoot, "base"), env.Namespace, service.Name, identity)
}

// consumedManagedServices returns, in a stable order, the environment's managed
// services this service's pods dial. Only edges that constrain running count: a
// build or schema edge on a database is read by the toolchain that generates
// code, not by the workload, so stamping its identity onto the pod would
// authenticate a container that never opens the connection.
func consumedManagedServices(service *resources.Service, env *resources.Environment) []string {
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

// soleWorkloadIdentity returns the one runtime identity a service's managed
// dependencies declare. A pod runs under a single ServiceAccount, so two managed
// endpoints naming different principals have no rendering: whichever was stamped
// last would win and the other endpoint would refuse the workload at runtime
// with nothing in the deploy to show for it. Several endpoints reached as the
// same principal are one identity and render as one.
func soleWorkloadIdentity(service string, consumed []string, env *resources.Environment) (*resources.EnvironmentWorkloadIdentity, error) {
	var identity *resources.EnvironmentWorkloadIdentity
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

func projectRenderedManagedIdentity(
	ctx context.Context,
	stage string,
	env *resources.Environment,
	graph map[string]*resources.Service,
) error {
	if len(env.ManagedServices) == 0 {
		return nil
	}
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
			if err := projectManagedIdentity(
				ctx,
				filepath.Join(servicesRoot, serviceEntry.Name()),
				service,
				env,
			); err != nil {
				return fmt.Errorf("project service %s managed identity: %w", serviceEntry.Name(), err)
			}
		}
	}
	return nil
}
