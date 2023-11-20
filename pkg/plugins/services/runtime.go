package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/plugins"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	v1 "github.com/codefly-dev/cli/proto/v1/services"
	runtimev1 "github.com/codefly-dev/cli/proto/v1/services/runtime"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

type InformationStatus = runtimev1.InformationResponse_Status

const (
	Unknown       = runtimev1.InformationResponse_UNKNOWN
	Init          = runtimev1.InformationResponse_INIT
	Started       = runtimev1.InformationResponse_STARTED
	RestartWanted = runtimev1.InformationResponse_RESTART_WANTED
	SyncWanted    = runtimev1.InformationResponse_SYNC_WANTED
	Stopped       = runtimev1.InformationResponse_STOPPED
	Error         = runtimev1.InformationResponse_ERROR
)

type IRuntime interface {
	Init(req *v1.InitRequest) (*runtimev1.InitResponse, error)

	Configure(req *runtimev1.ConfigureRequest) (*runtimev1.ConfigureResponse, error)

	Start(req *runtimev1.StartRequest) (*runtimev1.StartResponse, error)
	Information(req *runtimev1.InformationRequest) (*runtimev1.InformationResponse, error)

	Stop(req *runtimev1.StopRequest) (*runtimev1.StopResponse, error)

	Communicate(req *corev1.Engage) (*corev1.InformationRequest, error)
}

type ServiceRuntime struct {
	client runtimev1.RuntimeClient
	plugin *configurations.Plugin
}

type ServiceRuntimePluginContext struct {
}

func (m ServiceRuntimePluginContext) Key(p *configurations.Plugin, unique string) string {
	return p.Key(configurations.PluginFactoryService, unique)
}

func (m ServiceRuntimePluginContext) Default() plugin.Plugin {
	return &ServiceRuntimePlugin{}
}

// Configure documents things
// It can be used safely anywhere: doesn't start or do anything
func (m *ServiceRuntime) Configure(req *runtimev1.ConfigureRequest) (*runtimev1.ConfigureResponse, error) {
	return m.client.Configure(context.Background(), req)
}

// Init initializes the service
func (m *ServiceRuntime) Init(req *v1.InitRequest) (*runtimev1.InitResponse, error) {
	resp, err := m.client.Init(context.Background(), req)
	if err != nil && strings.Contains(err.Error(), "Marshal called with nil") {
		return resp, fmt.Errorf("WE PROBABLY HAVE A PANIC")
	}
	return resp, err
}

// Start starts the service
func (m *ServiceRuntime) Start(req *runtimev1.StartRequest) (*runtimev1.StartResponse, error) {
	resp, err := m.client.Start(context.Background(), req)
	if err != nil {
		st := status.Convert(err)
		for _, detail := range st.Details() {
			switch t := detail.(type) {
			case *errdetails.DebugInfo:
				return nil, shared.ParseError(t.Detail)
			}
		}
	}
	return resp, err
}

// Information return some useful information about the service
func (m *ServiceRuntime) Information(req *runtimev1.InformationRequest) (*runtimev1.InformationResponse, error) {
	return m.client.Information(context.Background(), req)
}

// Stop stops the service
func (m *ServiceRuntime) Stop(req *runtimev1.StopRequest) (*runtimev1.StopResponse, error) {
	return m.client.Stop(context.Background(), req)
}

// Communicate helper
func (m *ServiceRuntime) Communicate(req *corev1.Engage) (*corev1.InformationRequest, error) {
	return m.client.Communicate(context.Background(), req)
}

type ServiceRuntimePlugin struct {
	// GRPCPlugin must still implement the Plugin interface
	plugin.Plugin
	Runtime IRuntime
}

func (p *ServiceRuntimePlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	runtimev1.RegisterRuntimeServer(s, &RuntimeServer{Runtime: p.Runtime})
	return nil
}

func (p *ServiceRuntimePlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &ServiceRuntime{client: runtimev1.NewRuntimeClient(c)}, nil
}

// RuntimeServer wraps the gRPC protocol Request/Response
type RuntimeServer struct {
	runtimev1.UnimplementedRuntimeServer
	Runtime IRuntime
}

func (m *RuntimeServer) Configure(ctx context.Context, req *runtimev1.ConfigureRequest) (*runtimev1.ConfigureResponse, error) {
	return m.Runtime.Configure(req)
}

func (m *RuntimeServer) Init(ctx context.Context, req *v1.InitRequest) (*runtimev1.InitResponse, error) {
	return m.Runtime.Init(req)
}

func (m *RuntimeServer) Start(ctx context.Context, req *runtimev1.StartRequest) (*runtimev1.StartResponse, error) {
	return m.Runtime.Start(req)
}

func (m *RuntimeServer) Information(ctx context.Context, req *runtimev1.InformationRequest) (*runtimev1.InformationResponse, error) {
	return m.Runtime.Information(req)
}

func (m *RuntimeServer) Stop(ctx context.Context, req *runtimev1.StopRequest) (*runtimev1.StopResponse, error) {
	return m.Runtime.Stop(req)
}

func (m *RuntimeServer) Communicate(ctx context.Context, req *corev1.Engage) (*corev1.InformationRequest, error) {
	return m.Runtime.Communicate(req)
}

/*
Loader
*/

type ServiceRuntimeLoader struct {
	Logger hclog.Logger
}

func LoadRuntime(service *configurations.Service, opts ...plugins.Option) (*ServiceRuntime, error) {
	logger := shared.NewLogger("services.LoadRuntime")
	if service == nil || service.Plugin == nil {
		return nil, logger.Errorf("plugin cannot be nil")
	}
	runtime, err := plugins.Load[ServiceRuntimePluginContext, ServiceRuntime](
		service.Plugin.Of(configurations.PluginRuntimeService),
		service.Unique(),
		opts...)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot load service runtime plugin")
	}
	runtime.plugin = service.Plugin
	return runtime, nil
}

func NewRuntimePlugin(conf *configurations.Plugin, runtime IRuntime) plugins.PluginImplementation {
	return plugins.PluginImplementation{
		Configuration: conf,
		Plugin:        &ServiceRuntimePlugin{Runtime: runtime},
	}
}
