package services

import (
	"context"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/configurations"
	factoryv1 "github.com/codefly-dev/core/generated/v1/go/proto/services/factory"
	"github.com/codefly-dev/core/shared"
)

type Syncer struct {
	ApplicationConfiguration *configurations.Application
}

func NewSyncer(app *configurations.Application) (*Syncer, error) {
	return &Syncer{ApplicationConfiguration: app}, nil
}

func (r *Syncer) Sync(ctx context.Context, conf *configurations.Service) error {
	logger := shared.GetLogger(ctx).With("services.Syncer.Sync<%s>", conf.Name)
	logger.Debugf("refreshing service")

	f, err := services.LoadFactory(ctx, conf)
	if err != nil {
		return logger.Errorf("cannot load factory: %v", err)
	}

	//domain := r.ApplicationConfiguration.ServiceDomain(conf.Name)
	//
	//// Always eInit the factory instance
	//_, err = f.Init(ctx, &basev1.InitRequest{
	//	Identity: &basev1.ServiceIdentity{
	//		Name:      conf.Name,
	//		Domain:    domain,
	//		Namespace: conf.Namespace,
	//	},
	//})
	//if err != nil {
	//	return logger.Wrapf(err, "cannot initialize factory")
	//}

	_, err = f.Sync(ctx, &factoryv1.SyncRequest{
		// Address: path.Join(r.ApplicationConfiguration.NewDir(), conf.Name),
	})
	if err != nil {
		return logger.Errorf("cannot sync service: %v", err)
	}
	return nil
}
