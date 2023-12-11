package partial

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/cli/pkg/cli/display"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type Partial struct {
	Configuration *configurations.Partial
	Applications  []*application.Application
}

func NewPartial(ctx context.Context, project *configurations.Project, partial *configurations.Partial, mode application.Mode) (*Partial, error) {
	logger := shared.GetLogger(ctx).With("partial.NewPartial<%s>", partial.Name)
	configurations.SetMode(configurations.ModePartial)
	display.PartialLoading(partial)
	p := &Partial{Configuration: partial}
	for _, name := range partial.Applications {
		logger.Debugf("Loading application: %s", name)
		// Get config
		//config, err := configurations.LoadApplicationFromName(name)
		//if err != nil {
		//	return nil, logger.Wrapf(err, "failed to load application configuration: %s", name)
		//}
		//app, err := application.Load(project, config, mode)
		//if err != nil {
		//	return nil, logger.Wrapf(err, "failed to load application: %s", config)
		//}
		//err = p.Add(app)
		//if err != nil {
		//	return nil, logger.Wrapf(err, "failed to add application: %s", config)
		//}

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

func (p *Partial) Deploy(ctx context.Context, env *configurations.Environment) error {
	var errs []error
	for _, app := range p.Applications {
		if err := app.Deploy(ctx, env); err != nil {
			errs = append(errs, err)
		}
	}
	return shared.MultiErrors(errs...)

}
