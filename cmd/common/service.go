package common

import (
	"context"
	"errors"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
)

func ParseServiceArgument(ctx context.Context, project *configurations.Project, args []string) (*configurations.Service, error) {
	w := wool.Get(ctx).In("parseServiceArgument")
	if len(args) == 0 {
		return nil, nil
	}
	svcWithApp, err := configurations.ParseService(args[0])
	if err != nil {
		return nil, w.Wrap(err)
	}
	if svcWithApp.Application == "" {
		// If unique in project, we are good
		w.Debug("Looking for service", wool.NameField(svcWithApp.Name))
		svc, err := project.FindUniqueService(ctx, svcWithApp.Name)
		if err == nil {
			w.Debug("Found service", wool.ThisField(svc))
			return svc, nil
		}
		if errors.Is(err, configurations.NonUniqueServiceNameError{}) {
			// We must be in an application folder
			app := Application(ctx)
			if app == nil {
				return nil, w.NewError("Found multiple services with the same name: either run in application folder or specify application with service name like 'app/service'")
			}
			svc, err = app.LoadServiceFromName(ctx, svcWithApp.Name)
			if err != nil {
				return nil, w.Wrap(err)
			}
		} else {
			return nil, w.Wrap(err)
		}
		return nil, w.Wrap(err)
	}
	svc, err := project.LoadService(ctx, svcWithApp)
	if err != nil {
		return nil, w.Wrap(err)
	}
	return svc, nil
}
