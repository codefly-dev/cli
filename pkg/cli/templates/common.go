package templates

import "github.com/codefly-dev/core/resources"

type Display struct {
	*resources.Workspace
	*resources.Module
	*resources.Service
	Other map[string]interface{}
}

func New() *Display {
	return &Display{Other: make(map[string]interface{})}
}

func (d *Display) With(key string, value interface{}) *Display {
	d.Other[key] = value
	return d
}

func (d *Display) WithWorkspace(workspace *resources.Workspace) *Display {
	d.Workspace = workspace
	return d
}

func (d *Display) WithModule(module *resources.Module) *Display {
	d.Module = module
	return d
}

func (d *Display) WithService(service *resources.Service) *Display {
	d.Service = service
	return d
}
