package services

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/plugins"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	v1 "github.com/codefly-dev/cli/proto/v1/services"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

type IFactory interface {
	Init(req *v1.InitRequest) (*factoryv1.InitResponse, error)

	Create(req *factoryv1.CreateRequest) (*factoryv1.CreateResponse, error)
	Update(req *factoryv1.UpdateRequest) (*factoryv1.UpdateResponse, error)

	Sync(req *factoryv1.SyncRequest) (*factoryv1.SyncResponse, error)

	Build(req *factoryv1.BuildRequest) (*factoryv1.BuildResponse, error)
	Deploy(req *factoryv1.DeploymentRequest) (*factoryv1.DeploymentResponse, error)

	Communicate(req *corev1.Engage) (*corev1.InformationRequest, error)
}

type ServiceFactory struct {
	client factoryv1.FactoryClient
	plugin *configurations.Plugin
}

type ServiceFactoryPluginContext struct {
}

func (m ServiceFactoryPluginContext) Key(p *configurations.Plugin, unique string) string {
	return p.Key(configurations.PluginFactoryService, unique)
}

func (m ServiceFactoryPluginContext) Default() plugin.Plugin {
	return &ServiceFactoryPlugin{}
}

/*

 */

func (m ServiceFactory) Init(req *v1.InitRequest) (*factoryv1.InitResponse, error) {
	return m.client.Init(context.Background(), req)
}

func (m ServiceFactory) Create(req *factoryv1.CreateRequest) (*factoryv1.CreateResponse, error) {
	return m.client.Create(context.Background(), req)
}

func (m ServiceFactory) Update(req *factoryv1.UpdateRequest) (*factoryv1.UpdateResponse, error) {
	return m.client.Update(context.Background(), req)
}

func (m ServiceFactory) Sync(req *factoryv1.SyncRequest) (*factoryv1.SyncResponse, error) {
	return m.client.Sync(context.Background(), req)
}

func (m ServiceFactory) Build(req *factoryv1.BuildRequest) (*factoryv1.BuildResponse, error) {
	return m.client.Build(context.Background(), req)
}

func (m ServiceFactory) Deploy(req *factoryv1.DeploymentRequest) (*factoryv1.DeploymentResponse, error) {
	return m.client.Deploy(context.Background(), req)
}

func (m ServiceFactory) Communicate(req *corev1.Engage) (*corev1.InformationRequest, error) {
	return m.client.Communicate(context.Background(), req)
}

type ServiceFactoryPlugin struct {
	// GRPCPlugin must still implement the Plugin interface
	plugin.Plugin
	Factory IFactory
}

func (p *ServiceFactoryPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	factoryv1.RegisterFactoryServer(s, &FactoryServer{Factory: p.Factory})
	return nil
}

func (p *ServiceFactoryPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &ServiceFactory{client: factoryv1.NewFactoryClient(c)}, nil
}

// FactoryServer wraps the gRPC protocol Request/Response
type FactoryServer struct {
	factoryv1.UnimplementedFactoryServer
	Factory IFactory
}

func (m *FactoryServer) Init(ctx context.Context, req *v1.InitRequest) (*factoryv1.InitResponse, error) {
	return m.Factory.Init(req)
}

func (m *FactoryServer) Create(ctx context.Context, req *factoryv1.CreateRequest) (*factoryv1.CreateResponse, error) {
	return m.Factory.Create(req)
}

func (m *FactoryServer) Update(ctx context.Context, req *factoryv1.UpdateRequest) (*factoryv1.UpdateResponse, error) {
	return m.Factory.Update(req)
}

func (m *FactoryServer) Sync(ctx context.Context, req *factoryv1.SyncRequest) (*factoryv1.SyncResponse, error) {
	return m.Factory.Sync(req)
}

func (m *FactoryServer) Build(ctx context.Context, req *factoryv1.BuildRequest) (*factoryv1.BuildResponse, error) {
	return m.Factory.Build(req)
}

func (m *FactoryServer) Deploy(ctx context.Context, req *factoryv1.DeploymentRequest) (*factoryv1.DeploymentResponse, error) {
	return m.Factory.Deploy(req)
}

func (m *FactoryServer) Communicate(ctx context.Context, req *corev1.Engage) (*corev1.InformationRequest, error) {
	return m.Factory.Communicate(req)
}

func LoadFactory(conf *configurations.Service) (*ServiceFactory, error) {
	if conf == nil {
		return nil, fmt.Errorf("conf cannot be nil")
	}
	if conf.Plugin == nil {
		return nil, shared.NewLogger("services.LoadFactory<%s>", conf.Name).Errorf("plugin found nil")
	}
	logger := shared.NewLogger("services.LoadFactory<%s>", conf.Plugin.Name())
	logger.Debugf("loading service factory")
	factory, err := plugins.Load[ServiceFactoryPluginContext, ServiceFactory](conf.Plugin.Of(configurations.PluginFactoryService), conf.Unique())
	if err != nil {
		return nil, logger.Wrapf(err, "cannot load service factory conf")
	}
	factory.plugin = conf.Plugin
	return factory, nil
}

func NewFactoryPlugin(conf *configurations.Plugin, factory IFactory) plugins.PluginImplementation {
	return plugins.PluginImplementation{
		Configuration: conf,
		Plugin:        &ServiceFactoryPlugin{Factory: factory},
	}
}
