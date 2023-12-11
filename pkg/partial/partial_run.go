package partial

import (
	"context"
	"sync"

	"github.com/codefly-dev/cli/pkg/application"
	"github.com/codefly-dev/core/shared"
)

// Run runs the applications of the partial
// Each application run is blocking so we need to go-routine them
func (p *Partial) Run(ctx context.Context) error {
	logger := shared.GetLogger(ctx).With("partial.Run")
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
