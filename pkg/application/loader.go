package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli/display"
	"github.com/codefly-dev/cli/pkg/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/golor"
)

type Mode string

const (
	RuntimeMode Mode = "runtime"
	FactoryMode Mode = "factory"
)

type Loader struct {
	application *configurations.Application
	project     *configurations.Project

	plan []*services.Instance

	configurations map[string]*configurations.Service
	references     map[string]*configurations.ServiceReference
	entries        map[string]*configurations.ServiceDependency

	publicDependencies map[string][]*configurations.ServiceDependency

	mode Mode

	graph   *Graph
	verbose bool
}

func (l *Loader) Load(ctx context.Context) (*Application, error) {
	w := wool.Get(ctx).In("applications.Loader<%s::%s>", l.application.Name, l.project.Name)
	display.ApplicationLoading(l.application)
	logger.Debugf("loading application")
	for _, ref := range l.application.Services {
		if ref.Application == "" {
			ref.Application = l.application.Name
		}
		logger.Debugf("loading service %v/%v", ref.Application, ref.Name)
		svc, err := l.LoadServiceConfiguration(ctx, ref)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot load service <%s>", ref.Name)
		}
		l.configurations[ref.Name] = svc
		l.references[ref.Name] = ref
	}

	// Order of services
	order := l.graph.TopologicalSort()
	logger.Debugf("services in application: %v", order)

	if l.Verbose() {
		golor.Println(`#(blue,bold)[Running services]:
{{- range .Services}}
- #(cyan,bold)[{{.}}]{{end}}`, map[string]any{"Services": order})
	}

	for _, name := range order {
		conf := l.configurations[name]
		svc, err := services.NewServiceInstance(conf, l.application)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot create service")
		}
		logger.Debugf("loaded service <%s> from agent: %v", svc.Configuration.Name, conf.Agent.Identifier())
		l.plan = append(l.plan, svc)
	}
	app, err := NewApplication(ctx, l)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create applications")
	}
	switch l.mode {
	case FactoryMode:
		err = app.FactoryInit(ctx)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot init applications for factory")
		}
	case RuntimeMode:
		err = app.RuntimeInit(ctx)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot init applications for runtime")
		}
	default:
		return nil, logger.Errorf("unknown mode <%s>", l.mode)

	}

	if l.Verbose() {
		app.MakeVerbose()
	}
	app.PublicDependencies = l.publicDependencies
	return app, nil
}

func (l *Loader) LoadServiceConfiguration(ctx context.Context, ref *configurations.ServiceReference) (*configurations.Service, error) {
	//w := wool.Get(ctx).In("application.Loader.LoadServiceConfiguration<%s>", ref.Name)
	//name := ref.Name
	//if svc, ok := l.configurations[name]; ok {
	//	logger.Tracef("service <%s> already loaded", name)
	//	return svc, nil
	//}
	////app := l.application
	//if ref.Application != l.application.Name {
	//	//other, err := configurations.LoadApplicationFromName(ref.Application)
	//	//if err != nil {
	//	//	return nil, logger.Wrapf(err, "cannot load application <%s>", ref.Application)
	//	//}
	//}
	//service, err := configurations.LoadServiceFromReference(ref)
	//if err != nil {
	//	return nil, logger.Wrapf(err, "cannot load service application for %v", ref)
	//}
	//l.references[name] = ref
	//l.configurations[name] = service
	//unique := service.Unique()
	//
	//logger.Tracef("loaded service <%s>", name)
	//l.graph.AddNode(name)
	//for _, dep := range service.Dependencies {
	//	if dep.Application == "" {
	//		// For convenience -- wil not be saved
	//		dep.Application = l.application.Name
	//	}
	//	l.publicDependencies[unique] = append(l.publicDependencies[unique], dep)
	//	if !BelongToSameApplication(dep, l.application) {
	//		// Only load in-application dependencies
	//		continue
	//	}
	//	_, err := l.LoadServiceConfiguration(ctx, dep.AsReference())
	//	if err != nil {
	//		return nil, logger.Wrapf(err, "cannot load service <%s>", dep.Name)
	//	}
	//	l.graph.AddEdge(dep.Name, name)
	//}
	//return service, nil
	return nil, nil
}

func BelongToSameApplication(dep *configurations.ServiceDependency, app *configurations.Application) bool {
	return dep.Application == app.Name
}

func (app *Application) Belong(dep *configurations.ServiceDependency) bool {
	return BelongToSameApplication(dep, app.Configuration)
}

func (l *Loader) Verbose() bool {
	return l.verbose
}

func NewLoader(project *configurations.Project, app *configurations.Application, mode Mode) (*Loader, error) {
	return &Loader{
		application:        app,
		project:            project,
		mode:               mode,
		entries:            make(map[string]*configurations.ServiceDependency),
		publicDependencies: make(map[string][]*configurations.ServiceDependency),
		references:         make(map[string]*configurations.ServiceReference),
		configurations:     make(map[string]*configurations.Service),
		graph:              NewGraph(app.Name),
	}, nil
}

func (l *Loader) MakeVerbose() {
	l.verbose = true
}
