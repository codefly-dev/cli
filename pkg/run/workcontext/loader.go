package workcontext

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/pkg/environments"
)

// WithKeySet layers this run's work-context group over a loader, for the
// composition that authors none.
//
// It belongs on the loader rather than beside the per-service configurations
// a run hands out: the group is a declared workspace-configuration dependency,
// and those are resolved from the manager's own loaded set
// (configurations.Manager.GetWorkspaceDependenciesConfigurations). A group
// handed alongside that set is never found by it, and the service fails to
// boot with "no configuration found for work-context".
//
// An authored group always wins. A composition with a host declares where its
// key set is served; this answers only for the one that does not.
func WithKeySet(inner configurations.Loader, authority *Authority) configurations.Loader {
	return &keySetLoader{Loader: inner, authority: authority}
}

type keySetLoader struct {
	configurations.Loader
	authority *Authority
	// configurations is built once: the Manager resolves secret references in
	// the returned objects IN PLACE and reads them back, so handing out fresh
	// copies would discard what it resolved.
	configurations []*basev0.Configuration
}

func (l *keySetLoader) Load(ctx context.Context, env *resources.Environment) error {
	if err := l.Loader.Load(ctx, env); err != nil {
		return err
	}
	loaded := l.Loader.Configurations()
	if l.authority == nil || named(loaded, environments.WorkContextConfiguration) {
		l.configurations = loaded
		return nil
	}
	l.configurations = append(append([]*basev0.Configuration{}, loaded...), &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name: environments.WorkContextConfiguration,
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: environments.WorkContextKeySetURLKey, Value: l.authority.KeySetURL()},
				{Key: environments.WorkContextIssuerKey, Value: l.authority.Issuer()},
			},
		}},
	})
	return nil
}

func (l *keySetLoader) Configurations() []*basev0.Configuration {
	if l.configurations == nil {
		return l.Loader.Configurations()
	}
	return l.configurations
}

// CompositionRootWorkspaceConfigurationNames forwards the optional capability
// the wrapped loader may carry. Swallowing it would leave the Manager unable to
// tell which groups come from the composition root, which it injects into every
// service in the run.
func (l *keySetLoader) CompositionRootWorkspaceConfigurationNames() []string {
	if inner, ok := l.Loader.(interface {
		CompositionRootWorkspaceConfigurationNames() []string
	}); ok {
		return inner.CompositionRootWorkspaceConfigurationNames()
	}
	return nil
}

// AmbiguousWorkspaceConfigurations forwards the loader's other optional
// capability, for the same reason.
func (l *keySetLoader) AmbiguousWorkspaceConfigurations() map[string]error {
	if inner, ok := l.Loader.(interface {
		AmbiguousWorkspaceConfigurations() map[string]error
	}); ok {
		return inner.AmbiguousWorkspaceConfigurations()
	}
	return nil
}

func named(configs []*basev0.Configuration, group string) bool {
	for _, configuration := range configs {
		for _, info := range configuration.GetInfos() {
			if info.GetName() == group {
				return true
			}
		}
	}
	return false
}
