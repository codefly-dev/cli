package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	runtimev1 "github.com/codefly-dev/core/generated/v1/go/proto/services/runtime"
	"github.com/codefly-dev/core/shared"
)

type ServiceEnvironment struct {
	name     string
	mappings []*runtimev1.NetworkMapping
	replicas map[string][]*runtimev1.NetworkMapping
}

func (e *ServiceEnvironment) Add(service *services.Instance, mappings []*runtimev1.NetworkMapping) {
	if service.IsReplica() {
		e.replicas[service.Name()] = append(e.replicas[service.Name()], mappings...)
		return
	}
	e.mappings = mappings
}

func NewServiceEnvironment(ctx context.Context, name string) *ServiceEnvironment {
	return &ServiceEnvironment{
		name: name, replicas: make(map[string][]*runtimev1.NetworkMapping),
	}
}

type Environment struct {
	application string
	services    map[string]*ServiceEnvironment
}

func (e *Environment) AddNetworkMappings(ctx context.Context, service *services.Instance, mappings []*runtimev1.NetworkMapping) error {
	name := service.Name() // Identical for replicas and original
	if _, ok := e.services[name]; !ok {
		e.services[name] = NewServiceEnvironment(ctx, name)
	}
	e.services[name].Add(service, mappings)
	return nil
}

func (e *Environment) NetworkMappingsFor(ctx context.Context, name string) ([]*runtimev1.NetworkMapping, error) {
	logger := shared.GetLogger(ctx).With("NetworkMappingsFor")
	if env, ok := e.services[name]; ok {
		return env.mappings, nil
	}
	return nil, logger.Errorf("cannot find service environment for %s", name)
}

func NetworkMappingsFor(ctx context.Context, refs []*configurations.ServiceDependency) ([]*runtimev1.NetworkMapping, error) {
	logger := shared.GetLogger(ctx).With("NetworkMappingsFor")
	var mappings []*runtimev1.NetworkMapping
	for _, ref := range refs {
		env := GetEnvironment(ref.Application)
		if env == nil {
			return nil, logger.Errorf("cannot find environment for application <%s>", ref.Application)
		}
		mps, err := env.NetworkMappingsFor(ctx, ref.Name)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot get network mappings for service <%s>", ref.Name)
		}
		mappings = append(mappings, mps...)
	}
	return mappings, nil
}

var environments map[string]*Environment

func init() {
	environments = make(map[string]*Environment)
}

func GetEnvironment(app string) *Environment {
	if env, ok := environments[app]; ok {
		return env
	}
	return nil
}

func NewEnvironment(ctx context.Context, app string) (*Environment, error) {
	env := &Environment{
		application: app,
		services:    make(map[string]*ServiceEnvironment),
	}
	environments[app] = env
	return env, nil
}
