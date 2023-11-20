package management

import (
	"fmt"

	managementv1 "github.com/codefly-dev/cli/proto/v1/management"
	"github.com/codefly-dev/core/configurations"
)

type PluginsManager struct {
	bases map[string]*PluginManager
}

type Usage struct {
	Project     string
	Application string
	Service     string
	Version     string
}

type PluginManager struct {
	uses []*Usage
}

func (m *PluginManager) Add(app *configurations.Application, service *configurations.Service) {
	m.uses = append(m.uses, &Usage{
		Project:     app.Project,
		Application: app.Name,
		Version:     service.Version,
	})
}

func (m *PluginManager) Uses() []*managementv1.Usage {
	var uses []*managementv1.Usage
	for _, u := range m.uses {
		uses = append(uses, &managementv1.Usage{
			Application: u.Application,
			Service:     u.Service,
		})
	}
	return uses
}

func NewPluginManager() *PluginManager {
	return &PluginManager{}
}

func NewPluginsManager() *PluginsManager {
	return &PluginsManager{bases: make(map[string]*PluginManager)}
}

func (m *PluginsManager) AddPlugin(app *configurations.Application, service *configurations.Service) {
	base := fmt.Sprintf("%s/%s", service.Plugin.Publisher, service.Plugin.Identifier)
	if _, ok := m.bases[base]; !ok {
		m.bases[base] = NewPluginManager()
	}
	m.bases[base].Add(app, service)
}

func (m *PluginsManager) Usage() map[string]*managementv1.PluginUsage {
	usage := make(map[string]*managementv1.PluginUsage)
	for base := range m.bases {
		usage[base] = &managementv1.PluginUsage{}
	}
	return usage
}
