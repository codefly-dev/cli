package plugins

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/hashicorp/go-plugin"
)

type Plugin struct {
	Configuration  *configurations.Plugin
	Type           string
	Implementation plugin.Plugin
}

var HandshakeConfig = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "codefly::plugins",
	MagicCookieValue: "0.0.0",
}

type PluginImplementation struct {
	Plugin        plugin.Plugin
	Configuration *configurations.Plugin
}

func Register(plugins ...PluginImplementation) {
	pluginMap := make(map[string]plugin.Plugin)
	for _, p := range plugins {
		pluginMap[p.Configuration.Unique()] = p.Plugin
	}
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins:         pluginMap,
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

func ClearPlugins() {
	plugin.CleanupClients()
}
