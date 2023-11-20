package services

import (
	v1 "github.com/codefly-dev/cli/proto/v1/services"
	factoryv1 "github.com/codefly-dev/cli/proto/v1/services/factory"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type Syncer struct {
	ApplicationConfiguration *configurations.Application
}

func NewSyncer(app *configurations.Application) (*Syncer, error) {
	return &Syncer{ApplicationConfiguration: app}, nil
}

func (r *Syncer) Sync(conf *configurations.Service) error {
	logger := shared.NewLogger("services.Syncer.Sync<%s>", conf.Name)
	logger.Debugf("refreshing service")

	f, err := LoadFactory(conf)
	if err != nil {
		return logger.Errorf("cannot load factory: %v", err)
	}

	domain := r.ApplicationConfiguration.ServiceDomain(conf.Name)

	// Always eInit the factory instance
	_, err = f.Init(&v1.InitRequest{
		Identity: &v1.ServiceIdentity{
			Name:      conf.Name,
			Domain:    domain,
			Namespace: conf.Namespace,
		},
	})
	if err != nil {
		return logger.Wrapf(err, "cannot initialize factory")
	}

	_, err = f.Sync(&factoryv1.SyncRequest{
		// Address: path.Join(r.ApplicationConfiguration.NewDir(), conf.Name),
	})
	if err != nil {
		return logger.Errorf("cannot sync service: %v", err)
	}
	return nil
}
