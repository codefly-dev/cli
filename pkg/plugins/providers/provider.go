package providers

import (
	"context"

	"github.com/codefly-dev/cli/pkg/plugins"
	providerv1 "github.com/codefly-dev/cli/proto/v1/providers"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

type IProvider interface {
	Init(req *providerv1.InitRequest) (*providerv1.InitResponse, error)
}

type Provider struct {
	client providerv1.ProviderClient
	plugin configurations.Plugin
}

func (m Provider) Name() string {
	return m.plugin.Name()
}

func (m Provider) Key(s string) string {
	return m.plugin.Key(configurations.PluginProvider, s)
}

func (m Provider) Default() plugin.Plugin {
	return &ProviderPlugin{}
}

func (m Provider) Init(req *providerv1.InitRequest) (*providerv1.InitResponse, error) {
	return m.client.Init(context.Background(), req)
}

type ProviderPlugin struct {
	// GRPCPlugin must still implement the Plugin interface
	plugin.Plugin
	Provider IProvider
}

func (p *ProviderPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	providerv1.RegisterProviderServer(s, &ProviderServer{Provider: p.Provider})
	return nil
}

func (p *ProviderPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &Provider{client: providerv1.NewProviderClient(c)}, nil
}

// ProviderServer wraps the gRPC protocol Request/Response
type ProviderServer struct {
	providerv1.UnimplementedProviderServer
	Provider IProvider
}

func (m *ProviderServer) Init(ctx context.Context, req *providerv1.InitRequest) (*providerv1.InitResponse, error) {
	return m.Provider.Init(req)
}

func LoadProvider(plugin *configurations.Plugin) (*Provider, error) {
	logger := shared.NewLogger("providers.LoadProvider<%s>", plugin.Name())
	if plugin == nil {
		return nil, logger.Errorf("plugin cannot be nil")
	}
	return nil, nil
	//logger.Debugf("loading")
	//provider, err := plugins.Load[Provider](plugin.Of(configurations.PluginProvider), plugin.Unique())
	//if err != nil {
	//	return nil, logger.Wrapf(err, "cannot load provider provider plugin")
	//}
	//provider.plugin = *plugin
	//return provider, nil
}

func NewPlugin(conf *configurations.Plugin, provider IProvider) plugins.PluginImplementation {
	return plugins.PluginImplementation{
		Configuration: conf,
		Plugin:        &ProviderPlugin{Provider: provider},
	}
}
