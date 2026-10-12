package environments

import (
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// Deployed reports whether this environment is a hosted cell rather than a
// local cluster. It is the split the CLI already draws with IsK3d — a k3d
// cluster is a developer's own machine, anything else is a cluster other people
// reach — read here under the name the rules that depend on it are written in,
// so a rule meant for a cell never fires on a laptop.
func (env *Environment) Deployed() bool {
	if env == nil {
		return false
	}
	return !env.IsK3d()
}

// IngressRoutesFor returns the routes this environment declares for one
// endpoint, in declared order. A route matches when it names the service (by
// bare name or module/service unique) and either names this endpoint or is
// service-wide (empty Endpoint). The per-endpoint field is honored so a route
// meant for one endpoint is not read as another's.
func IngressRoutesFor(env *Environment, module, service, endpoint string) []EnvironmentIngressRoute {
	if env == nil {
		return nil
	}
	unique := resources.ServiceUnique(module, service)
	var matched []EnvironmentIngressRoute
	for _, route := range env.Ingress {
		if route.Service != service && route.Service != unique {
			continue
		}
		if route.Endpoint != "" && route.Endpoint != endpoint {
			continue
		}
		matched = append(matched, route)
	}
	return matched
}

// IngressHosts returns the hosts this environment's routes bind to one endpoint,
// in declared order. It is the render of a route's own question — which hosts
// does the edge answer for — and is deliberately separate from PublicOriginFor,
// which answers what the workload is told it is reached as.
func IngressHosts(env *Environment, module, service, endpoint string) []string {
	var hosts []string
	seen := map[string]struct{}{}
	for _, route := range IngressRoutesFor(env, module, service, endpoint) {
		for _, host := range route.Hosts {
			if _, ok := seen[host]; ok {
				continue
			}
			seen[host] = struct{}{}
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// PublicOriginFor resolves the public origin this environment fixes for one
// exposed endpoint, or refuses by name.
//
// The declaration is resolved, never merely counted. Two forms answer it: an
// ingress route naming hosts — the first host is the endpoint's canonical
// origin, so an operator writes the origin the product answers as first and its
// aliases after — or, with no route, the host derived from the environment's
// declared app host suffix, the same derivation an external endpoint's DNS
// record takes.
//
// Anything else is a refusal, and so is a declaration that exists but resolves
// to nothing: an empty route, a route whose hosts are empty or blank, and a host
// that is not a usable web origin (ResolveOrigin). The presence of a declaration
// has never been the question — a workload is handed an origin or it is not.
//
// Only an endpoint that is ALLOCATED an outward address is asked. Which ones
// those are is core's `exposure`, not a visibility: a visibility is reach and
// says nothing about an address, so an endpoint every module may call but
// nothing outside addresses has no origin to fix and is never asked for one.
func PublicOriginFor(env *Environment, module, service, endpoint string) (string, error) {
	subject := publicEndpointName(module, service, endpoint)
	routes := IngressRoutesFor(env, module, service, endpoint)
	for _, route := range routes {
		if len(route.Hosts) == 0 {
			continue
		}
		origin, err := ResolveOrigin(route.Hosts[0])
		if err != nil {
			return "", describeOriginRefusal(fmt.Sprintf("the host declared for %s", subject), err)
		}
		return origin, nil
	}
	if len(routes) > 0 {
		return "", fmt.Errorf("the ingress route declared for %s names no host it answers as", subject)
	}
	if host := env.AppHost(&resources.ServiceIdentity{Name: service, Module: module}); host != "" {
		origin, err := ResolveOrigin(host)
		if err != nil {
			return "", describeOriginRefusal(fmt.Sprintf("the host derived for %s from dns.app-host-suffix", subject), err)
		}
		return origin, nil
	}
	return "", fmt.Errorf(
		"%s is allocated an address reachable from outside the workspace (exposure %q) but has no public origin: declare an ingress route naming the host it answers as, or an app host suffix under dns.app-host-suffix",
		subject, resources.ExposurePublic)
}

// publicEndpointName is how an endpoint is named in a refusal.
func publicEndpointName(module, service, endpoint string) string {
	return resources.ServiceUnique(module, service) + "/" + endpoint
}

// ValidateIngress refuses a route whose declaration cannot answer the question a
// route exists to answer, independently of any composition: one that names no
// service, one that names no host, one whose hosts are blank, and one whose host
// is not a usable web origin.
//
// The composition-wide question — does every exposed endpoint resolve to an
// origin — is answered where the composition is known. This is the half that can
// be answered from the declaration alone, so it is answered on every path that
// reads an environment rather than only on a render.
func (env *Environment) ValidateIngress() error {
	if env == nil {
		return nil
	}
	for index, route := range env.Ingress {
		position := route.Name
		if position == "" {
			position = fmt.Sprintf("entry %d", index+1)
		}
		if strings.TrimSpace(route.Service) == "" {
			return fmt.Errorf("ingress route %s names no service", position)
		}
		if len(route.Hosts) == 0 {
			return fmt.Errorf("ingress route %s names no host it answers as", position)
		}
		for _, host := range route.Hosts {
			if strings.TrimSpace(host) == "" {
				return fmt.Errorf("ingress route %s names a blank host", position)
			}
			if _, err := ResolveOrigin(host); err != nil {
				return describeOriginRefusal(fmt.Sprintf("the host declared on ingress route %s", position), err)
			}
		}
	}
	return env.SolutionBoundary.validate()
}
