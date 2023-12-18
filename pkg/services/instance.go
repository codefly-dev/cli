package services

import (
	"context"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/configurations"

	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	factoryv1 "github.com/codefly-dev/core/generated/go/services/factory/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
)

type Instance struct {
	Configuration *configurations.Service
	Application   *configurations.Application

	// Reference to master to handle replica
	Reference *configurations.ServiceReference

	Location string

	ServiceIdentity *basev1.ServiceIdentity

	Runtime *services.RuntimeAgent
	Factory *services.FactoryAgent

	Initialized bool
	Ready       bool
	Started     bool

	ReplicaOf   *configurations.Service
	Quiet       bool
	Persistence bool
}

type Mode string

const (
	Factory Mode = "factory"
	Runtime Mode = "runtime"
)

func NewServiceInstance(conf *configurations.Service, app *configurations.Application) (*Instance, error) {
	//w := wool.Get(ctx).In("service.Instance<%s>", conf.Name)
	//ref, err := conf.Reference()
	//if err != nil {
	//	return nil, w.Wrapf(err, "cannot get reference")
	//}
	//id := configurations.Identity(conf)
	//identity := &servicev1.ServiceIdentity{
	//	Name:        id.Name,
	//	Application: id.Application,
	//	Domain:      id.Domain,
	//	Namespace:   id.Namespace,
	//}
	//location := path.Join(app.Dir(opts...), conf.Name)
	//instance := &Instance{
	//	w:        w,
	//	Configuration: conf,
	//	Application:   app,
	//
	//	ServiceIdentity: identity,
	//	Location:        location,
	//
	//	Quiet:                      ref.RunningOptions.Quiet,
	//	Persistence:                ref.RunningOptions.Persistence,
	//	CommunicationServerManager: communicate.NewServerManager(w),
	//	Reference:                  ref,
	//}
	//err = instance.LoadFactory()
	//if err != nil {
	//	return nil, w.Wrapf(err, "cannot create factory")
	//}
	//err = instance.LoadRuntime()
	//if err != nil {
	//	return nil, w.Wrapf(err, "cannot create runtime")
	//}
	//return instance, nil
	return nil, nil
}

func (s *Instance) LoadFactory(ctx context.Context) error {
	w := wool.Get(ctx).In("Instance::LoadFactory", wool.ThisField(s))
	factory, err := services.LoadFactory(ctx, s.Configuration)
	if err != nil {
		return w.Wrapf(err, "cannot load factory")
	}
	s.Factory = factory
	return nil
}

func (s *Instance) LoadRuntime(ctx context.Context) error {
	w := wool.Get(ctx).In("Instance::LoadFactory", wool.ThisField(s))
	runtime, err := services.LoadRuntime(ctx, s.Configuration)
	if err != nil {
		return w.Wrapf(err, "cannot load factory")
	}
	s.Runtime = runtime
	return nil
}

func (s *Instance) Unique() string {
	return s.Configuration.Unique()
}

/*
Factory wrapper
*/

func (s *Instance) SoloFactoryInit(ctx context.Context) error {
	//// Need some refactoring between Instance and Service in Application
	//req := &servicev1.InitRequest{
	//	Debug:    shared.IsDebug(),
	//	Location: s.Location,
	//	Identity: s.ServiceIdentity,
	//}
	//_, err := s.FactoryInit(ctx, req)
	//return err
	return nil
}

func (s *Instance) FactoryInit(ctx context.Context, r *runtimev1.InitRequest) (*factoryv1.InitResponse, error) {
	return nil, nil
}

func (s *Instance) Update(ctx context.Context, r *factoryv1.UpdateRequest) (*factoryv1.UpdateResponse, error) {
	return s.Factory.Update(ctx, r)
}

func (s *Instance) Create(ctx context.Context, r *factoryv1.CreateRequest) (*factoryv1.CreateResponse, error) {
	return nil, nil

}

func (s *Instance) Sync(ctx context.Context, r *factoryv1.SyncRequest) (*factoryv1.SyncResponse, error) {
	return nil, nil
}

func (s *Instance) Build(ctx context.Context, r *factoryv1.BuildRequest) (*factoryv1.BuildResponse, error) {
	return s.Factory.Build(ctx, r)
}

func (s *Instance) Deploy(ctx context.Context, r *factoryv1.DeploymentRequest) (*factoryv1.DeploymentResponse, error) {
	return s.Factory.Deploy(ctx, r)
}

func (s *Instance) Name() string {
	if s.IsReplica() {
		return s.ReplicaOf.Name
	}
	return s.Configuration.Name
}

func (s *Instance) IsReplica() bool {
	return s.ReplicaOf != nil
}

/*
Runtime wrapper
*/
