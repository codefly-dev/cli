package gitops

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// THE ONE EXCEPTION TO "ONLY THE HOP", AND WHY IT IS THE SERVICE'S TO STATE.
//
// Behind a hop, a module endpoint admits the hop and nothing else (hop.go).
// Some listeners are legitimately not called through it: a worker that pulls
// from a store in its own module, a webhook receiver a third party posts to
// from inside the cell, a sidecar's scrape port. Those callers are not
// consumers of a published API and no hop routes them.
//
// The declaration is the SERVICE's, on the endpoint it serves, and it names
// the admitted caller: `admitted-sources` in service.codefly.yaml, an entry
// per endpoint with the services admitted to it.
//
//	admitted-sources:
//	  - endpoint: ingest
//	    from: [relay/webhook]
//
// It is never implied from a dependency graph. A consumer declaring an edge to
// a module service is exactly what the hop rule refuses; if an edge admitted
// itself, the rule would admit everything it exists to refuse. So the target
// says who may reach it, which is also the only side that knows why — "this
// port is not an API call" is a fact about the listener.
//
// Core does not model the key. A service manifest is an open schema and keeps
// an unmodelled top-level key in its inline ExtraFields, round-tripping it
// through a save, so the declaration lives on the manifest with no core
// change. It is read HERE, where the reach is derived, and refused by name:
// an endpoint the service does not declare, a service the composition does
// not carry, a repeated endpoint or caller, and the service naming itself
// (a service reaches its own endpoints under its own principal, which the
// RENDER declares when it renders a non-serving workload — not the manifest).
const admittedSourcesKey = "admitted-sources"

// admittedSource is one endpoint of a service and the services admitted to it
// besides the hop.
type admittedSource struct {
	// Endpoint is an endpoint this service declares.
	Endpoint string `yaml:"endpoint"`
	// From are the `<module>/<service>` callers admitted to it.
	From []string `yaml:"from"`
}

// declaredAdmittedSources reads a service's `admitted-sources`, keyed by
// endpoint name, with every caller sorted and each once. A service that
// declares none returns nil.
//
// module is the module the service renders in: a caller named without one
// resolves there, as a service dependency does.
func declaredAdmittedSources(module string, service *resources.Service) (map[string][]string, error) {
	raw, declared := service.ExtraFields[admittedSourcesKey]
	if !declared {
		return nil, nil
	}
	unique := resources.ServiceUnique(module, service.Name)
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%s of %s: %s", admittedSourcesKey, unique, fmt.Sprintf(format, args...))
	}
	// A key written with nothing under it is NOT "no declaration": reading it
	// that way is how a mistyped or half-written admission would silently
	// become the hop-only default with nothing said.
	if raw == nil {
		return nil, refuse("it is declared and admits nobody; remove the key, or name the endpoint and its callers")
	}
	encoded, err := yaml.Marshal(raw)
	if err != nil {
		return nil, refuse("%v", err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(encoded)))
	decoder.KnownFields(true)
	var entries []admittedSource
	if err = decoder.Decode(&entries); err != nil {
		return nil, refuse("it is a list of {endpoint, from} entries (%v)", err)
	}
	if len(entries) == 0 {
		return nil, refuse("it is declared and admits nobody; remove the key, or name the endpoint and its callers")
	}
	sources := map[string][]string{}
	for _, entry := range entries {
		if entry.Endpoint == "" {
			return nil, refuse("an entry names no endpoint; each entry is one endpoint of this service and the callers admitted to it")
		}
		if _, repeated := sources[entry.Endpoint]; repeated {
			return nil, refuse("endpoint %s is named twice; one entry states every caller admitted to an endpoint", entry.Endpoint)
		}
		if !slices.ContainsFunc(service.Endpoints, func(e *resources.Endpoint) bool { return e != nil && e.Name == entry.Endpoint }) {
			return nil, refuse("endpoint %s is not one this service declares", entry.Endpoint)
		}
		if len(entry.From) == 0 {
			return nil, refuse("endpoint %s admits nobody; drop the entry — an endpoint with no entry admits the hop alone", entry.Endpoint)
		}
		callers := make([]string, 0, len(entry.From))
		for _, from := range entry.From {
			caller := strings.TrimSpace(from)
			if caller == "" {
				return nil, refuse("endpoint %s admits an unnamed caller", entry.Endpoint)
			}
			if !strings.Contains(caller, "/") {
				caller = resources.ServiceUnique(module, caller)
			}
			if strings.Count(caller, "/") != 1 {
				return nil, refuse("endpoint %s admits %q, which is not a <module>/<service> of the composition", entry.Endpoint, from)
			}
			if caller == unique {
				return nil, refuse("endpoint %s admits %s, the service that serves it: a service reaches its own endpoints under its own principal, and the RENDER declares that when it renders a workload that does not serve them", entry.Endpoint, caller)
			}
			callers = append(callers, caller)
		}
		sort.Strings(callers)
		if compacted := slices.Compact(slices.Clone(callers)); len(compacted) != len(callers) {
			return nil, refuse("endpoint %s admits a caller twice", entry.Endpoint)
		}
		sources[entry.Endpoint] = callers
	}
	return sources, nil
}
