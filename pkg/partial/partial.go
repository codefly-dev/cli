package partial

import (
	"context"
	"fmt"
	"sync"

	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/cli/display"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type Partial struct {
	Configuration *configurations.Partial
	Applications  []*application.Application
}

func NewPartial(project *configurations.Project, partial *configurations.Partial, mode application.Mode) (*Partial, error) {
	logger := shared.NewLogger("partial.NewPartial<%s>", partial.Name)
	configurations.SetMode(configurations.ModePartial)
	display.PartialLoading(partial)
	p := &Partial{Configuration: partial}
	for _, name := range partial.Applications {
		logger.Debugf("Loading application: %s", name)
		// Get config
		config, err := configurations.LoadApplicationFromName(name)
		if err != nil {
			return nil, logger.Wrapf(err, "failed to load application configuration: %s", name)
		}
		app, err := application.Load(project, config, mode)
		if err != nil {
			return nil, logger.Wrapf(err, "failed to load application: %s", config)
		}
		err = p.Add(app)
		if err != nil {
			return nil, logger.Wrapf(err, "failed to add application: %s", config)
		}

	}

	return p, nil
}

func (p *Partial) Configure(ctx context.Context) error {
	var errs []error
	for _, app := range p.Applications {
		if err := app.Configure(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return shared.MultiErrors(errs...)
}

// Run runs the applications of the partial
// Each application run is blocking so we need to go-routine them
func (p *Partial) Run(ctx context.Context) error {
	logger := shared.NewLogger("partial.Run")
	for _, app := range p.Applications {
		err := app.Configure(ctx)
		if err != nil {
			return logger.Wrapf(err, "cannot configure application")
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error)
	for _, app := range p.Applications {
		logger.Debugf("Loading application: %s", app.Configuration.Name)
		wg.Add(1)
		go func(app *application.Application) {
			defer wg.Done()
			if err := app.Run(ctx); err != nil {
				errs <- err
			}
		}(app)
	}
	wg.Wait()
	return nil
}

func (p *Partial) Add(app *application.Application) error {
	if app == nil {
		return fmt.Errorf("got a nil app")
	}
	p.Applications = append(p.Applications, app)
	return nil
}

func (p *Partial) Stop(ctx context.Context) error {
	var errs []error
	for _, app := range p.Applications {
		if err := app.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return shared.MultiErrors(errs...)
}

func (p *Partial) Sync(ctx context.Context) error {
	var errs []error
	for _, app := range p.Applications {
		if err := app.Sync(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return shared.MultiErrors(errs...)
}

func (p *Partial) Build(ctx context.Context) error {
	var errs []error
	for _, app := range p.Applications {
		if err := app.Build(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return shared.MultiErrors(errs...)
}
