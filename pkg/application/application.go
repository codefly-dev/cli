package application

import (
	"context"
	"fmt"
	"strings"

	services2 "github.com/codefly-dev/core/agents/services"

	"github.com/codefly-dev/cli/pkg/monitoring"
	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/agents"
	runtimev1 "github.com/codefly-dev/core/proto/v1/go/services/runtime"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type Application struct {
	Configuration *configurations.Application
	Project       *configurations.Project

	// We run services in order
	Plan *Plan

	IsConfigured  bool
	IsInitialized bool

	// Track running services
	ServiceTracker *monitoring.ServiceTracker

	// Load services runtime
	ServiceLoader *services2.ServiceRuntimeLoader

	// TODO: clarify which one does what...
	Environment     *Environment
	EndpointManager *ApplicationEndpointManager

	// Application loader helper: analyze dependencies, etc...
	Loader *Loader

	// internals
	verbose bool

	events  chan monitoring.ServiceEvent
	uniques map[string]*services.Instance

	// Other applications in the project
	dependencies       []*Application
	PublicDependencies map[string][]*configurations.ServiceDependency
}

type Plan struct {
	Services []*services.Instance
}

func (p *Plan) Show() string {
	var names []string
	for _, service := range p.Services {
		names = append(names, service.Configuration.Name)
	}
	return fmt.Sprintf("[%s]", strings.Join(names, ", "))
}

func NewApplication(loader *Loader) (*Application, error) {
	logger := shared.NewLogger("applications.NewApplication<%s>", loader.application.Name)
	events := make(chan monitoring.ServiceEvent)
	serviceTracker, err := monitoring.NewServiceTracker(events)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create service tracker")
	}
	plan := &Plan{
		Services: loader.plan,
	}
	logger.Debugf("loaded application with plan: %s", plan.Show())

	app := &Application{
		Configuration: loader.application,
		Project:       loader.project,

		Plan: plan,

		ServiceTracker: serviceTracker,
		events:         events,

		// internals

		// Mapping to services
		// Note: never loop over maps
		uniques: make(map[string]*services.Instance),

		EndpointManager: NewApplicationEndpointManager(loader.application),
	}
	go app.Listen()
	return app, nil
}

func (app *Application) Restart(unique string) error {
	service, ok := app.uniques[unique]
	if !ok {
		return fmt.Errorf("unknow service unique identifier: %s", unique)
	}
	logger := shared.NewLogger("applications.Restart<%s>", service.Configuration.Name)

	golor.Println(`#(bold,cyan)[Restarting {{.Name}}]`, map[string]any{"Name": service.Configuration.Name})

	ctx := context.Background()
	if !app.uniques[unique].Ready {
		err := app.RuntimeInitService(service)
		if err != nil {
			return logger.Wrapf(err, "cannot init service")
		}
	}
	if app.uniques[unique].Started {
		logger.Debugf("stopping service")
		err := app.StopService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot stop service")
		}
		logger.Debugf("configuring service")
		err = app.ConfigureService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot configure service")
		}
	}

	logger.Debugf("start service")
	err := app.StartService(ctx, service)
	if err != nil {
		return logger.Wrapf(err, "cannot start service")
	}
	return nil
}

func (app *Application) Stop(ctx context.Context) error {
	logger := shared.NewLogger("applications.Stop<%s>", app.Configuration.Name)
	logger.Debugf("stopping")
	var exitOrder []*services.Instance
	exitOrder = append(exitOrder, app.Plan.Services...)

	Reverse(exitOrder)
	for _, service := range exitOrder {
		if _, ok := app.uniques[service.Configuration.Unique()]; !ok {
			logger.Debugf("service <%s> is not running", service.Configuration.Name)
			continue
		}
		err := app.StopService(ctx, service)
		if err != nil {
			return logger.Wrapf(err, "cannot stop service <%s>", service.Configuration.Name)
		}
	}
	return nil
}

func (app *Application) StopService(ctx context.Context, service *services.Instance) error {
	logger := shared.NewLogger("applications.StopService<%s>", service.Configuration.Name)
	if service.Runtime == nil {
		return logger.Errorf("runtime for service <%s> is not initialized, run first app.Init()", service.Configuration.Name)
	}
	if app.uniques[service.Configuration.Unique()].Ready {
		return nil
	}
	// Stop the runtime
	_, err := service.Runtime.Stop(&runtimev1.StopRequest{
		Persist: service.Persistence,
	})
	if err != nil {
		return logger.Wrapf(err, "cannot stop runtime")
	}
	// Untrack
	logger.Debugf("Stopping tracker for service <%s>", service.Configuration.Name)
	err = app.ServiceTracker.Untrack(service.Configuration)
	if err != nil {
		return logger.Wrapf(err, "cannot untrack service")
	}
	agents.Cleanup(service.Configuration.Unique())
	return nil
}

func (app *Application) Listen() {
	logger := shared.NewLogger("applications.Listen<%s>", app.Configuration.Name)
	for event := range app.events {
		switch event.Event {
		case "RestartWanted":
			err := app.Restart(event.Unique)
			if err != nil {
				logger.Oops("cannot restart service <%s>: %v", event.Unique, err)
			}
		}
	}
}

func (app *Application) CanRun() bool {
	return len(app.Plan.Services) > 0
}

func (app *Application) AddDependency(other *Application) {
	app.dependencies = append(app.dependencies, other)
}

func (app *Application) MakeVerbose() {
	app.verbose = true
}

func Load(project *configurations.Project, app *configurations.Application, mode Mode) (*Application, error) {
	logger := shared.NewLogger("application.Load<%s/%s>", project.Name, app.Name)
	logger.Debugf("calling loader")
	loader, err := NewLoader(project, app, mode)
	if err != nil {
		return nil, err
	}
	return loader.Load()
}
