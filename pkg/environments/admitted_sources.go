package environments

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// AN ADMISSION TAKES TWO SIGNATURES, AND THIS IS THE OPERATOR'S.
//
// Behind a hop, a module endpoint admits the hop and nothing else: the whole
// reason to stand a hop is that a module's own code never has to install the
// check, so its listeners must have no other way to receive traffic. Some
// listeners are legitimately not called through it — a worker pulling from a
// store, a webhook receiver, a scrape port — and the SERVICE declares those on
// the endpoint it serves (`admitted-sources` in service.codefly.yaml, read by
// pkg/gitops).
//
// That declaration alone is module code. A waiver of the permission layer
// written in the repository the waiver protects is a waiver anyone who can land
// a commit in that module can grant themselves, which is exactly the property
// the hop exists to hold: a client has no way to reach a module except through
// the accepted permission layer, and nothing depends on module code doing auth
// correctly. So the manifest's declaration PROPOSES an admission and this one
// ACCEPTS it: an admission takes effect only where the environment — which the
// operator owns, not the module — co-declares the same service, endpoint and
// caller. A manifest admission with no co-declaration here is a render error
// naming what is missing, never a quietly dropped or a quietly granted waiver,
// and every effective admission is written into the render record so the
// applier and a reviewer see the whole waiver set without reading manifests.
//
//	environments:
//	  - name: production
//	    admitted-sources:
//	      - service: relay/ingest
//	        endpoint: ingest
//	        from: [other/webhook]
//
// This half states no reason: the service's own manifest is where "this port is
// not an API call" is said, because the listener is the only side that knows
// it. Here the operator says who may do it in THIS environment, which is also
// why the two halves are not one file — a cell admits a caller a developer's
// own cluster need not, and the reverse.

// EnvironmentAdmittedSource is the operator's acceptance of one service's
// admission: the `<module>/<service>` that serves the endpoint, the endpoint,
// and the callers admitted to it in this environment.
type EnvironmentAdmittedSource struct {
	// Service is the `<module>/<service>` serving the endpoint.
	Service string `yaml:"service"`
	// Endpoint is the endpoint of that service the callers are admitted to.
	Endpoint string `yaml:"endpoint"`
	// From are the `<module>/<service>` callers this environment admits.
	From []string `yaml:"from"`
}

// AdmitsSource reports whether this environment co-declares one admission:
// the caller, on that endpoint of that service. Nothing is implied — an
// environment that declares none admits none, and the render refuses the
// manifest's proposal rather than rendering it.
func (env *Environment) AdmitsSource(service, endpoint, caller string) bool {
	if env == nil {
		return false
	}
	for index := range env.AdmittedSources {
		entry := &env.AdmittedSources[index]
		if entry.Service == service && entry.Endpoint == endpoint && slices.Contains(entry.From, caller) {
			return true
		}
	}
	return false
}

// AdmittedSourceCallers are the callers this environment co-declares on one
// endpoint of one service, sorted and each once.
func (env *Environment) AdmittedSourceCallers(service, endpoint string) []string {
	if env == nil {
		return nil
	}
	var callers []string
	for index := range env.AdmittedSources {
		entry := &env.AdmittedSources[index]
		if entry.Service != service || entry.Endpoint != endpoint {
			continue
		}
		callers = append(callers, entry.From...)
	}
	sort.Strings(callers)
	return slices.Compact(callers)
}

// validateAdmittedSources holds every entry to the shape an admission is
// written in, refusing by name what the render would otherwise have to guess
// at. It judges the SHAPE alone: whether the composition carries the service,
// the endpoint and the caller is judged where the reach is derived, against
// the manifest that proposed the admission.
func (env *Environment) validateAdmittedSources() error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("environment %s: admitted-sources: %s", env.Name, fmt.Sprintf(format, args...))
	}
	seen := map[string]bool{}
	for index := range env.AdmittedSources {
		entry := &env.AdmittedSources[index]
		if err := validateAdmittedSourceUnique("service", entry.Service, refuse); err != nil {
			return err
		}
		if strings.TrimSpace(entry.Endpoint) == "" {
			return refuse("the entry for %s names no endpoint; each entry is one endpoint of one service and the callers this environment admits to it", entry.Service)
		}
		key := entry.Service + "/" + entry.Endpoint
		if seen[key] {
			return refuse("%s is named twice; one entry states every caller this environment admits to an endpoint", key)
		}
		seen[key] = true
		if len(entry.From) == 0 {
			return refuse("%s admits nobody; remove the entry — an endpoint with no entry admits the hop alone", key)
		}
		callers := make([]string, 0, len(entry.From))
		for _, from := range entry.From {
			if err := validateAdmittedSourceUnique("caller of "+key, from, refuse); err != nil {
				return err
			}
			if from == entry.Service {
				return refuse("%s admits %s, the service that serves it: a service reaches its own endpoints under its own principal, which the render declares when it renders a workload that does not serve them", key, from)
			}
			callers = append(callers, from)
		}
		sort.Strings(callers)
		if compacted := slices.Compact(slices.Clone(callers)); len(compacted) != len(callers) {
			return refuse("%s admits a caller twice", key)
		}
	}
	return nil
}

// validateAdmittedSourceUnique holds one name to `<module>/<service>`. An
// admission is never resolved against a default module: the environment is not
// the module's file, so a bare name has no module to mean.
func validateAdmittedSourceUnique(what, name string, refuse func(string, ...any) error) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return refuse("the %s is unnamed", what)
	}
	module, service, found := strings.Cut(trimmed, "/")
	if !found || module == "" || service == "" || strings.Contains(service, "/") {
		return refuse("the %s is %q, which is not a <module>/<service>", what, name)
	}
	if trimmed != resources.ServiceUnique(module, service) {
		return refuse("the %s is %q, which is not a <module>/<service>", what, name)
	}
	return nil
}
