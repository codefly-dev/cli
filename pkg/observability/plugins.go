package observability

import (
	"fmt"

	managementv1 "github.com/codefly-dev/cli/proto/v1/go/management"

	"github.com/codefly-dev/core/configurations"
)

type AgentsManager struct {
	bases map[string]*AgentManager
}

type Usage struct {
	Project     string
	Application string
	Service     string
	Version     string
}

type AgentManager struct {
	uses []*Usage
}

func (m *AgentManager) Add(app *configurations.Application, service *configurations.Service) {
	m.uses = append(m.uses, &Usage{
		Project:     app.Project,
		Application: app.Name,
		Version:     service.Version,
	})
}

func (m *AgentManager) Uses() []*managementv1.Usage {
	var uses []*managementv1.Usage
	for _, u := range m.uses {
		uses = append(uses, &managementv1.Usage{
			Application: u.Application,
			Service:     u.Service,
		})
	}
	return uses
}

func NewAgentManager() *AgentManager {
	return &AgentManager{}
}

func NewAgentsManager() *AgentsManager {
	return &AgentsManager{bases: make(map[string]*AgentManager)}
}

func (m *AgentsManager) AddAgent(app *configurations.Application, service *configurations.Service) {
	base := fmt.Sprintf("%s/%s", service.Agent.Publisher, service.Agent.Identifier())
	if _, ok := m.bases[base]; !ok {
		m.bases[base] = NewAgentManager()
	}
	m.bases[base].Add(app, service)
}

func (m *AgentsManager) Usage() map[string]*managementv1.AgentUsage {
	usage := make(map[string]*managementv1.AgentUsage)
	for base := range m.bases {
		usage[base] = &managementv1.AgentUsage{}
	}
	return usage
}
