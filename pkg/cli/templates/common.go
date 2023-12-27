package templates

import "github.com/codefly-dev/core/configurations"

type Display struct {
	*configurations.Project
	*configurations.Application
	*configurations.Service
	Other map[string]interface{}
}

func New() *Display {
	return &Display{Other: make(map[string]interface{})}
}

func (d *Display) With(key string, value interface{}) *Display {
	d.Other[key] = value
	return d
}

func (d *Display) WithProject(project *configurations.Project) *Display {
	d.Project = project
	return d
}

func (d *Display) WithApplication(application *configurations.Application) *Display {
	d.Application = application
	return d
}

func (d *Display) WithService(service *configurations.Service) *Display {
	d.Service = service
	return d
}
