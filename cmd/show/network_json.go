package show

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/remotenetwork"
	"github.com/codefly-dev/core/architecture"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
)

// networkReport is `show network --json`: per service, each endpoint's native
// address on a local run and its deployed in-cluster port for one environment.
type networkReport struct {
	Workspace   string                 `json:"workspace"`
	NamingScope string                 `json:"naming_scope"`
	Environment string                 `json:"environment"`
	Services    []networkServiceReport `json:"services"`
}

type networkServiceReport struct {
	// Service is <module>/<service>, the module being the name the workspace
	// composes it under — the name the in-cluster port hash takes.
	Service string `json:"service"`
	Module  string `json:"module"`
	Name    string `json:"name"`
	// Managed is true when the environment replaces the service with a managed
	// one: it has no in-cluster workload, so no endpoint has a deployed port.
	Managed bool `json:"managed,omitempty"`
	// Render is the module render, relative to the workspace, that the
	// endpoints' container ports were read from (--rendered); absent when the
	// service's module has no render or the render has no unit for it.
	Render       string                    `json:"render,omitempty"`
	Endpoints    []networkEndpointReport   `json:"endpoints"`
	Dependencies []networkDependencyReport `json:"dependencies,omitempty"`
}

type networkEndpointReport struct {
	Name       string `json:"name"`
	API        string `json:"api"`
	Visibility string `json:"visibility"`
	// Native is the address the endpoint binds on a local run; absent for an
	// external endpoint, which resolves through DNS at runtime.
	Native string `json:"native,omitempty"`
	// DeployedPort is the endpoint's in-cluster port in the environment; absent
	// when the render gives it none (a public host, or a managed service).
	DeployedPort *uint16 `json:"deployed_port,omitempty"`
	// ContainerPort is the port the endpoint's pods listen on, read from the
	// rendered manifests (--rendered): the targetPort of the Service port that
	// publishes the endpoint's in-cluster port. Absent without a render, or
	// when the render publishes no Service port for the endpoint.
	ContainerPort *uint32 `json:"container_port,omitempty"`
}

type networkDependencyReport struct {
	Service string `json:"service"`
	// Endpoints lists the endpoints consumed; empty means all of them.
	Endpoints []string `json:"endpoints,omitempty"`
}

func writeNetworkJSON(ctx context.Context, out io.Writer, workspace *resources.Workspace, deps *architecture.ServiceDependencies) error {
	env, err := orchestration.SelectEnvironment(workspace, showNetworkEnv)
	if err != nil {
		return err
	}
	remote, err := orchestration.NewEnvironmentRemoteManager(ctx, workspace)
	if err != nil {
		return fmt.Errorf("cannot prepare the in-cluster allocation: %w", err)
	}
	report := networkReport{
		Workspace:   workspace.Name,
		NamingScope: namingScope,
		Environment: env.Name,
		Services:    []networkServiceReport{},
	}
	for _, s := range deps.Services() {
		svc, err := deps.ServiceFromUnique(s.Unique)
		if err != nil {
			return fmt.Errorf("cannot load service %s: %w", s.Unique, err)
		}
		id, err := svc.Identity()
		if err != nil {
			return fmt.Errorf("cannot identify service %s: %w", s.Unique, err)
		}
		endpoints, err := svc.LoadEndpoints(ctx)
		if err != nil {
			return fmt.Errorf("cannot load endpoints of %s: %w", s.Unique, err)
		}
		service := networkServiceReport{
			Service:   id.Unique(),
			Module:    id.Module,
			Name:      id.Name,
			Endpoints: []networkEndpointReport{},
		}
		_, service.Managed = env.ManagedService(id.Module, id.Name)
		var deployed map[string]uint16
		if !service.Managed {
			if deployed, err = remote.DeployedPorts(ctx, env, id, endpoints); err != nil {
				return fmt.Errorf("cannot allocate in-cluster ports of %s in environment %s: %w", id.Unique(), env.Name, err)
			}
		}
		var container map[string]uint32
		if showNetworkRendered && !service.Managed {
			if container, service.Render, err = renderedContainerPorts(ctx, workspace, env, remote, id, endpoints); err != nil {
				return err
			}
		}
		for _, ep := range endpoints {
			entry := networkEndpointReport{Name: ep.Name, API: ep.Api, Visibility: ep.Visibility}
			if entry.Visibility == "" {
				entry.Visibility = "private"
			}
			if !resources.IsExternalEndpoint(ep) {
				if inst := network.NativeFor(ctx, workspace.Name, id.Module, id.Name, namingScope, ep); inst != nil {
					entry.Native = inst.Address
				}
			}
			if port, placed := deployed[ep.Name]; placed {
				entry.DeployedPort = &port
			}
			if port, rendered := container[ep.Name]; rendered {
				entry.ContainerPort = &port
			}
			service.Endpoints = append(service.Endpoints, entry)
		}
		for _, dep := range svc.ServiceDependencies {
			dependency := networkDependencyReport{Service: dep.Unique()}
			for _, e := range dep.Endpoints {
				dependency.Endpoints = append(dependency.Endpoints, e.Name)
			}
			service.Dependencies = append(service.Dependencies, dependency)
		}
		report.Services = append(report.Services, service)
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// renderedContainerPorts reads a service's container ports from its module's
// committed render, joining on the in-cluster ports the render hands the
// service's agent (the same RemoteManager.GenerateNetworkMappings a deploy
// calls). It returns the render's path relative to the workspace.
func renderedContainerPorts(
	ctx context.Context,
	workspace *resources.Workspace,
	env *environments.Environment,
	remote *remotenetwork.RemoteManager,
	id *resources.ServiceIdentity,
	endpoints []*basev0.Endpoint,
) (map[string]uint32, string, error) {
	mappings, err := remote.GenerateNetworkMappings(ctx, env, workspace, id, endpoints)
	if err != nil {
		return nil, "", fmt.Errorf("cannot map the in-cluster endpoints of %s in environment %s: %w", id.Unique(), env.Name, err)
	}
	ports, root, err := gitops.ServiceContainerPorts(workspace, id.Module, id.Name, env.Name, orchestration.InClusterPorts(ctx, mappings))
	if err != nil || root == "" {
		return nil, "", err
	}
	relative, err := filepath.Rel(workspace.Dir(), root)
	if err != nil {
		return nil, "", err
	}
	return ports, filepath.ToSlash(relative), nil
}
