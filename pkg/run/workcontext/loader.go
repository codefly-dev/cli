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
func WithKeySet(inner configurations.Loader, authority *Authority, declared []string) configurations.Loader {
	return &keySetLoader{Loader: inner, authority: authority, declared: declared}
}

type keySetLoader struct {
	configurations.Loader
	authority *Authority
	// declared names every workspace-configuration group some service in the
	// composition depends on. A declared group that nothing supplies is a boot
	// failure at the manager — GetWorkspaceDependenciesConfigurations refuses a
	// dependency it cannot find — whatever the service would do with empty
	// values. Locally there is no composition to supply the host's groups, so
	// they are routed in empty and each service decides what absence means,
	// which is the rule those services already document.
	declared []string
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
	layered := append([]*basev0.Configuration{}, loaded...)

	// Routing a declared group in empty is a LOCAL answer only. A deployed
	// composition that declares a group and supplies nothing has a
	// misconfiguration, and the manager refusing to start is how that is found;
	// covering it here would turn a loud boot failure into a service running
	// silently without the capability it said it needed.
	if local(env) {
		layered = append(layered, l.empties(loaded)...)
	}

	// The key set is added on its own terms: a composition that authors the
	// group keeps its own, and a run with no authority adds nothing. This is
	// deliberately NOT folded into the check above — the two were conjoined,
	// so a composition that authored work-context and nothing else had its
	// other declared groups silently skipped.
	if l.authority != nil && !named(loaded, environments.WorkContextConfiguration) {
		layered = append(layered, &basev0.Configuration{
			Origin: resources.ConfigurationWorkspace,
			Infos: []*basev0.ConfigurationInformation{{
				Name: environments.WorkContextConfiguration,
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: environments.WorkContextKeySetURLKey, Value: l.authority.KeySetURL()},
					{Key: environments.WorkContextIssuerKey, Value: l.authority.Issuer()},
				},
			}},
		})
	}
	l.configurations = layered
	return nil
}

// local reports whether this run is the local environment, which is the only
// one a missing declared group is answered for rather than refused.
func local(env *resources.Environment) bool {
	return env != nil && resources.Match(env.Name, resources.LocalEnvironment().Name)
}

// empties routes in every declared group the composition did not supply,
// carrying no values. It never covers a group that was supplied, and never the
// work-context group, which carries this run's own key set.
func (l *keySetLoader) empties(loaded []*basev0.Configuration) []*basev0.Configuration {
	var out []*basev0.Configuration
	for _, group := range l.declared {
		if group == environments.WorkContextConfiguration || named(loaded, group) || named(out, group) {
			continue
		}
		out = append(out, &basev0.Configuration{
			Origin: resources.ConfigurationWorkspace,
			Infos:  []*basev0.ConfigurationInformation{{Name: group}},
		})
	}
	return out
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
