package services

import (
	"context"
	"path"

	"github.com/codefly-dev/core/agents/services"

	"github.com/codefly-dev/core/agents/communicate"

	cli "github.com/codefly-dev/cli/pkg/cli/prompts/communicate"
	"github.com/codefly-dev/core/configurations"
	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"
	servicev1 "github.com/codefly-dev/core/proto/v1/go/services"
	factoryv1 "github.com/codefly-dev/core/proto/v1/go/services/factory"
	runtimev1 "github.com/codefly-dev/core/proto/v1/go/services/runtime"
	"github.com/codefly-dev/core/shared"
)

type Instance struct {
	Logger *shared.Logger

	Configuration *configurations.Service
	Application   *configurations.Application

	// Reference to master to handle replica
	Reference *configurations.ServiceReference

	Location string

	ServiceIdentity *servicev1.ServiceIdentity

	Runtime *services.ServiceRuntime
	Factory *services.ServiceFactory

	Initialized bool
	Ready       bool
	Started     bool

	ReplicaOf   *configurations.Service
	Quiet       bool
	Persistence bool

	// Communication helper
	CommunicationServerManager *communicate.ServerManager
}

type Mode string

const (
	Factory Mode = "factory"
	Runtime Mode = "runtime"
)

func NewServiceInstance(conf *configurations.Service, app *configurations.Application) (*Instance, error) {
	logger := shared.NewLogger("service.Instance<%s>", conf.Name)
	scope := configurations.WithScopeProjectOnly()
	scope.Application = app
	opts := []configurations.Option{configurations.WithApplication(app)}
	ref, err := conf.Reference()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot get reference")
	}
	id := configurations.Identity(conf)
	identity := &servicev1.ServiceIdentity{
		Name:        id.Name,
		Application: id.Application,
		Domain:      id.Domain,
		Namespace:   id.Namespace,
	}
	location := path.Join(app.Dir(opts...), conf.Name)
	instance := &Instance{
		Logger:        logger,
		Configuration: conf,
		Application:   app,

		ServiceIdentity: identity,
		Location:        location,

		Quiet:                      ref.RunningOptions.Quiet,
		Persistence:                ref.RunningOptions.Persistence,
		CommunicationServerManager: communicate.NewServerManager(logger),
		Reference:                  ref,
	}
	err = instance.LoadFactory()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create factory")
	}
	err = instance.LoadRuntime()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create runtime")
	}
	return instance, nil
}

func (s *Instance) LoadFactory() error {
	logger := shared.NewLogger("applications.LoadFactory<%s>", s.Unique())
	factory, err := services.LoadFactory(s.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot load factory")
	}
	s.Factory = factory
	return nil
}

func (s *Instance) LoadRuntime() error {
	logger := shared.NewLogger("applications.LoadRuntime<%s>", s.Unique())
	runtime, err := services.LoadRuntime(s.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot load factory")
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
	// Need some refactoring between Instance and Service in Application
	req := &servicev1.InitRequest{
		Debug:    shared.Debug(),
		Location: s.Location,
		Identity: s.ServiceIdentity,
	}
	_, err := s.FactoryInit(req)
	return err
}

func (s *Instance) FactoryInit(r *servicev1.InitRequest) (*factoryv1.InitResponse, error) {
	resp, err := s.Factory.Init(r)
	if err != nil {
		return nil, s.Logger.Wrapf(err, "cannot init factory")
	}
	err = s.CommunicationServerManager.Register(resp.Channels...)
	if err != nil {
		return nil, s.Logger.Wrapf(err, "cannot register channels")
	}
	return resp, nil
}

func (s *Instance) Update(r *factoryv1.UpdateRequest) (*factoryv1.UpdateResponse, error) {
	return s.Factory.Update(r)
}

func (s *Instance) Create(r *factoryv1.CreateRequest) (*factoryv1.CreateResponse, error) {
	if server, ok := s.CommunicationServerManager.RequiresCommunication(r); ok {
		handler := &cli.CliHandler{}
		s.Logger.Debugf("starting CREATE communication to fetch the information for the agent")
		var answer *agentsv1.Answer

		// Send a first message
		first, err := s.Factory.Create(r)
		if err != nil {
			return nil, s.Logger.Wrapf(err, "cannot sync")
		}
		if !first.NeedCommunication {
			return first, nil
		}
		s.Logger.Debugf("we need some communication!")

		for {
			s.Logger.Debugf("answer: %v", answer)
			eng, err := server.Communicate(answer)
			if err != nil {
				return nil, s.Logger.Wrapf(err, "cannot communicate CREATE from server")
			}

			s.Logger.Debugf("engagement: %v", eng)
			req, err := s.Factory.Communicate(eng)
			if err != nil {
				return nil, s.Logger.Wrapf(err, "cannot communicate CREATE from factory")
			}
			s.Logger.Debugf("information request by client: %v", req)

			if req.Done {
				s.Logger.Debugf("client is done")
				break
			}

			answer, err = handler.Process(req)
			if err != nil {
				return nil, s.Logger.Wrapf(err, "cannot process")
			}
		}
	} else {
		s.Logger.Debugf("no communication required")
	}
	return s.Factory.Create(r)
}

func (s *Instance) Sync(r *factoryv1.SyncRequest) (*factoryv1.SyncResponse, error) {
	if server, ok := s.CommunicationServerManager.RequiresCommunication(r); ok {
		handler := &cli.CliHandler{}
		s.Logger.Debugf("starting SYNC communication to fetch the information for the agent")
		var answer *agentsv1.Answer

		// Send a first message
		first, err := s.Factory.Sync(r)
		if err != nil {
			return nil, s.Logger.Wrapf(err, "cannot sync")
		}
		if !first.NeedCommunication {
			return first, nil
		}
		s.Logger.Debugf("we need some communication!")

		for {
			s.Logger.Debugf("answer: %v", answer)
			eng, err := server.Communicate(answer)
			if err != nil {
				return nil, s.Logger.Wrapf(err, "cannot communicate SYNC")
			}

			s.Logger.Debugf("engagement: %v", eng)
			req, err := s.Factory.Communicate(eng)
			if err != nil {
				return nil, s.Logger.Wrapf(err, "cannot communicate")
			}
			s.Logger.Debugf("information request by client: %v", req)

			if req.Done {
				s.Logger.Debugf("client is done")
				break
			}

			answer, err = handler.Process(req)
			if err != nil {
				return nil, s.Logger.Wrapf(err, "cannot process")
			}
		}
	}

	return s.Factory.Sync(r)
}

// SoloBuild should really be used for debugging only
func (s *Instance) SoloBuild(ctx context.Context) error {
	err := s.SoloFactoryInit(ctx)
	if err != nil {
		return s.Logger.Wrapf(err, "cannot init runtime")
	}
	req := &factoryv1.BuildRequest{}
	_, err = s.Build(req)
	return err
}

func (s *Instance) Build(r *factoryv1.BuildRequest) (*factoryv1.BuildResponse, error) {
	return s.Factory.Build(r)
}

func (s *Instance) Deploy(r *factoryv1.DeploymentRequest) (*factoryv1.DeploymentResponse, error) {
	return s.Factory.Deploy(r)
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

func (s *Instance) SoloRuntimeInit(ctx context.Context) error {
	// Need some refactoring between Instance and Service in Application
	req := &servicev1.InitRequest{
		Debug:    shared.Debug(),
		Location: s.Location,
		Identity: s.ServiceIdentity,
	}
	_, err := s.RuntimeInit(req)
	return err
}

func (s *Instance) RuntimeInit(r *servicev1.InitRequest) (*runtimev1.InitResponse, error) {
	logger := shared.NewLogger("applications.eInit<%s>", s.Unique())
	resp, err := s.Runtime.Init(r)
	if err != nil {
		return nil, s.Logger.Wrapf(err, "cannot init runtime")
	}
	if resp.Status.State != servicev1.InitStatus_READY {
		return nil, s.Logger.Errorf("runtime is not ready: %v", resp.Status.Message)

	}
	logger.Debugf("got channels: %v", resp.Channels)
	err = s.CommunicationServerManager.Register(resp.Channels...)
	if err != nil {
		return nil, s.Logger.Wrapf(err, "cannot register channels")
	}
	return resp, nil
}

func (s *Instance) Configure(r *runtimev1.ConfigureRequest) (*runtimev1.ConfigureResponse, error) {
	return s.Runtime.Configure(r)
}

func (s *Instance) Start(r *runtimev1.StartRequest) (*runtimev1.StartResponse, error) {
	logger := shared.NewLogger("applications.Start<%s>", s.Unique())
	logger.Tracef("starting!")
	return s.Runtime.Start(r)
}
