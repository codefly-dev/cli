package gitops

import (
	"context"
	"fmt"
	"slices"
	"strings"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
)

// runnableReach is the one reach the composition PREPARES rather than
// declares. A service that installs prepared runnable bindings (it declares
// the `runnable-bindings` workspace configuration group) calls the owner of
// every derived operation in the workspace, and no module declares the other:
// the owner publishes an operation, the executor installs it, and neither
// names a service dependency on the other. The reach is therefore read from
// the prepared index, which is the only statement of it.
//
// Both the cell file's consumer lists and the mesh edges read this, so an
// executor is admitted to an owner it calls by one derivation rather than two
// that can disagree — and never by a value typed into either output.
type runnableReach struct {
	// executors are every service installing prepared bindings, sorted by
	// module then service and each once.
	executors []runnableExecutor
	// owners holds `<module>/<service>/<endpoint>` for every endpoint a
	// prepared call targets.
	owners map[string]bool
}

// runnableExecutor is one service that installs prepared bindings.
type runnableExecutor struct {
	Module  string
	Service string
}

// Unique is the `<module>/<service>` the cell file names a consumer by.
func (e runnableExecutor) Unique() string {
	return resources.ServiceUnique(e.Module, e.Service)
}

// reaches reports whether a prepared binding calls this endpoint.
func (reach *runnableReach) reaches(module, service, endpoint string) bool {
	if reach == nil {
		return false
	}
	return reach.owners[resources.ServiceUnique(module, service)+"/"+endpoint]
}

// derivedRunnableReach indexes it over the whole workspace. The owner
// endpoint is the one a prepared call targets: the published endpoint for
// REST, the service's connect endpoint for a gRPC publication (a gRPC owner
// is called over Connect, as `codefly generate runnable-bindings` prepares
// it), the published one when it is itself connect.
//
// An operation whose owner publishes no such endpoint is refused here, by
// name: it would otherwise render a cell in which the executor's call is
// denied by the mesh, which the owner reports to nobody and the executor
// sees only as an owner that did not answer.
func derivedRunnableReach(ctx context.Context, workspace *resources.Workspace) (*runnableReach, error) {
	reach := &runnableReach{owners: map[string]bool{}}
	modules, err := workspace.LoadModules(ctx)
	if err != nil {
		return nil, err
	}
	for _, module := range modules {
		for _, serviceReference := range module.ServiceReferences {
			service, err := module.LoadServiceFromName(ctx, serviceReference.Name)
			if err != nil {
				return nil, fmt.Errorf("load service %s/%s to index what it executes: %w", module.Name, serviceReference.Name, err)
			}
			if slices.Contains(service.WorkspaceConfigurationDependencies, runnablespkg.RunnableBindingsGroup) {
				reach.executors = append(reach.executors, runnableExecutor{Module: module.Name, Service: service.Name})
			}
		}
	}
	if len(reach.executors) == 0 {
		return reach, nil
	}
	slices.SortFunc(reach.executors, func(a, b runnableExecutor) int {
		if a.Module != b.Module {
			return strings.Compare(a.Module, b.Module)
		}
		return strings.Compare(a.Service, b.Service)
	})
	reach.executors = slices.Compact(reach.executors)
	for _, module := range modules {
		derived, err := runnablespkg.LoadIndex(module.Dir())
		if err != nil {
			return nil, fmt.Errorf("module %s: %w", module.Name, err)
		}
		if derived == nil {
			continue
		}
		for i := range derived.Operations {
			operation := &derived.Operations[i]
			service, err := module.LoadServiceFromName(ctx, operation.Service)
			if err != nil {
				return nil, fmt.Errorf("module %s derived operation %s: %w", module.Name, operation.Name, err)
			}
			endpoints, err := service.LoadEndpoints(ctx)
			if err != nil {
				return nil, fmt.Errorf("module %s derived operation %s: %w", module.Name, operation.Name, err)
			}
			called, err := calledEndpoints(module.Name, operation, endpoints)
			if err != nil {
				return nil, err
			}
			for _, name := range called {
				reach.owners[resources.ServiceUnique(module.Name, operation.Service)+"/"+name] = true
			}
		}
	}
	return reach, nil
}

// calledEndpoints is the endpoint names a prepared call to this operation
// targets.
func calledEndpoints(module string, operation *runnablespkg.IndexEntry, endpoints []*basev0.Endpoint) ([]string, error) {
	var published *basev0.Endpoint
	for _, endpoint := range endpoints {
		if endpoint.GetName() == operation.Endpoint {
			published = endpoint
		}
	}
	if published == nil {
		return nil, fmt.Errorf("module %s derived operation %s: service %s publishes it on endpoint %s, which that service does not declare",
			module, operation.Name, operation.Service, operation.Endpoint)
	}
	if published.GetApi() != standards.GRPC {
		return []string{published.GetName()}, nil
	}
	var connect []string
	for _, endpoint := range endpoints {
		if endpoint.GetApi() == standards.CONNECT {
			connect = append(connect, endpoint.GetName())
		}
	}
	if len(connect) == 0 {
		return nil, fmt.Errorf("module %s derived operation %s: a gRPC operation is called over its service's connect endpoint, and service %s declares none",
			module, operation.Name, operation.Service)
	}
	slices.Sort(connect)
	return connect, nil
}
