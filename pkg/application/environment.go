package application

import (
	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	runtimev1 "github.com/codefly-dev/core/proto/v1/go/services/runtime"
	"github.com/codefly-dev/core/shared"
)

type ServiceEnvironment struct {
	logger   *shared.Logger
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

//
//func (e *ServiceEnvironment) NetworkMappingsFor(service *services.Instance) ([]*runtimev1.NetworkMapping, error) {
//	var mappings []*runtimev1.NetworkMapping
//	// For external services, we will append all the mappings
//	if e.name != service.Name() {
//		mappings = append(mappings, e.mappings...)
//		for _, rep := range e.replicas {
//			mappings = append(mappings, rep...)
//		}
//		return mappings, nil
//	}
//	//// the original, ignore replicas entirely
//	//if !service.IsReplica() {
//	//	return e.mappings
//	//}
//	//// for replica, use the reference of the original with the address of the replica
//	//for _, mapping := range e.mappings {
//	//	var addresses []string
//	//	for _, m := range e.replicas[service.Name()] {
//	//		addresses = append(addresses, m.Addresses...)
//	//	}
//	//	mappings = append(mappings, &runtimev1.NetworkMapping{
//	//		Unique: mapping.Unique,
//	//		Addresses: addresses,
//	//	})
//	//}
//	return mappings, nil
//}

func NewServiceEnvironment(name string, logger *shared.Logger) *ServiceEnvironment {
	return &ServiceEnvironment{
		logger: logger,
		name:   name, replicas: make(map[string][]*runtimev1.NetworkMapping),
	}
}

type Environment struct {
	logger      *shared.Logger
	application string
	services    map[string]*ServiceEnvironment
}

func (e *Environment) AddNetworkMappings(service *services.Instance, mappings []*runtimev1.NetworkMapping) error {
	e.logger.Debugf("adding network for %v %v #%d", service.Configuration.Name, service.IsReplica(), len(mappings))
	name := service.Name() // Identical for replicas and original
	if _, ok := e.services[name]; !ok {
		e.services[name] = NewServiceEnvironment(name, e.logger)
	}
	e.services[name].Add(service, mappings)
	return nil
}

func (e *Environment) NetworkMappingsFor(name string) ([]*runtimev1.NetworkMapping, error) {
	if env, ok := e.services[name]; ok {
		return env.mappings, nil
	}
	return nil, e.logger.Errorf("cannot find service environment for %s", name)
}

func NetworkMappingsFor(refs []*configurations.ServiceDependency) ([]*runtimev1.NetworkMapping, error) {
	logger := shared.NewLogger("NetworkMappingsFor")
	var mappings []*runtimev1.NetworkMapping
	for _, ref := range refs {
		env := GetEnvironment(ref.Application)
		if env == nil {
			return nil, logger.Errorf("cannot find environment for application <%s>", ref.Application)
		}
		mps, err := env.NetworkMappingsFor(ref.Name)
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

func NewEnvironment(app string) (*Environment, error) {
	env := &Environment{
		application: app,
		logger:      shared.NewLogger("NewEnvironment"),
		services:    make(map[string]*ServiceEnvironment),
	}
	environments[app] = env
	return env, nil
}
