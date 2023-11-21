package management

import (
	"fmt"

	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/plugins/endpoints"
	managementv1 "github.com/codefly-dev/cli/proto/v1/management"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type Workspace struct {
	Project      *managementv1.ProjectView
	PluginUsages map[string]*managementv1.PluginUsage
}

type Manager struct {
	apps   map[string]*application.Application
	plugin *PluginsManager
}

func NewManager() *Manager {
	return &Manager{
		apps:   make(map[string]*application.Application),
		plugin: NewPluginsManager(),
	}
}

func (m *Manager) Load() (*Workspace, error) {
	logger := shared.NewLogger("management.Load")
	w := Workspace{}
	project, err := configurations.CurrentProject()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot list projects")
	}
	w.Project, err = m.LoadProject(project)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot load project")
	}
	w.PluginUsages = m.plugin.Usage()
	return &w, nil
}

func (m *Manager) LoadProject(project *configurations.Project) (*managementv1.ProjectView, error) {
	logger := shared.NewLogger("management.LoadProject")
	var apps []*managementv1.ApplicationView
	for _, a := range project.Applications {
		config, err := configurations.LoadApplicationFromName(a.Name, configurations.WithProject(project))
		if err != nil {
			return nil, logger.Wrapf(err, "cannot load applications: %s", a.Name)
		}
		app, err := m.LoadApplication(config, project, application.FactoryMode)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot create applications: %s", config.Name)
		}
		apps = append(apps, app)
	}
	return &managementv1.ProjectView{
		Name:         project.Name,
		Applications: apps,
	}, nil
}

func (m *Manager) LoadApplication(config *configurations.Application, project *configurations.Project, mode application.Mode) (*managementv1.ApplicationView, error) {
	logger := shared.NewLogger("management.LoadApplication")
	application.ShowEndpointManagerState()
	var services []*managementv1.ServiceView
	// Much easier to load it
	app, err := application.Load(project, config, mode)
	if err != nil {
		return nil, err
	}
	m.apps[app.Configuration.Name] = app
	for _, service := range app.Plan.Services {
		service, err := m.LoadService(app, service.Configuration)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot create service: %s", config.Name)
		}
		services = append(services, service)
	}
	return &managementv1.ApplicationView{
		Application: config.Name,
		Services:    services,
	}, nil
}

func (m *Manager) LoadService(app *application.Application, service *configurations.Service) (*managementv1.ServiceView, error) {
	logger := shared.NewLogger("management.LoadServiceConfiguration<%s>", service.Name)
	var views []*managementv1.EndpointView
	es, err := application.GetEndpoints(service)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot get endpoints")
	}
	logger.DebugMe("found #%d endpoints of %s", len(es), service.Unique())
	for _, e := range es {

		views = append(views, &managementv1.EndpointView{
			Endpoint: endpoints.Light(e),
		})
	}
	m.plugin.AddPlugin(app.Configuration, service)
	return &managementv1.ServiceView{
		Service:   service.Name,
		Endpoints: views,
	}, nil
}

func (w *Workspace) GetProject() (*managementv1.ProjectView, error) {
	return w.Project, nil
}

func (w *Workspace) GetApplication(application string) (*managementv1.ApplicationView, error) {
	p, err := w.GetProject()
	if err != nil {
		return nil, err
	}
	for _, a := range p.Applications {
		if a.Application == application {
			return a, nil
		}
	}
	return nil, fmt.Errorf("applications not found")
}

func (w *Workspace) Usage(base string) (*managementv1.PluginUsage, error) {
	return w.PluginUsages[base], nil
}
