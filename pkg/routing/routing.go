// Package routing renders a service's public surface into Kubernetes edge
// routing manifests. The CLI already carries ingress *intent*
// (resources.EnvironmentIngressRoute); this package turns that intent, plus
// the deterministic in-cluster backend contract, into concrete routes that a
// solution's rendered output can install and uninstall as one unit.
//
// Emission is backend-neutral: an Exposure describes what to route, and a
// Renderer decides how. Two renderers ship today — "gateway-api" (Kubernetes
// Gateway API GRPCRoute/HTTPRoute, the current standard, implemented by Istio)
// and "istio" (the legacy networking.istio.io VirtualService envelope). A
// third backend (e.g. Envoy Gateway) is a new Renderer, not a caller change.
package routing

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/codefly-dev/core/standards"
)

// ExposedEndpoint is one public endpoint to route to a backend. Port, Hosts,
// and Prefix are per-endpoint: the in-cluster port is endpoint-specific (a
// named sibling does not share the canonical port), ingress binds hosts to a
// specific endpoint, and only a gRPC endpoint carries a proto-package prefix.
type ExposedEndpoint struct {
	// Name is the endpoint name (used to name generated objects).
	Name string
	// API is the codefly API kind (standards.GRPC/REST/HTTP/CONNECT).
	API string
	// Port is the in-cluster backend Service port for THIS endpoint. It must
	// be resolved with the same canonical-vs-named rule the network layer
	// uses (see cmd/expose inClusterPorts / core network.GenerateNetworkMappings):
	// only the canonical owner of a per-API port keeps standards.Port, so this
	// is not simply standards.Port(API).
	Port uint16
	// Hosts are the external hostnames this endpoint answers for, taken from
	// the ingress routes that name this endpoint (or a service-wide route).
	Hosts []string
	// Prefix is the gRPC proto package this endpoint serves, matched as
	// "<prefix>.*". It scopes a GRPCRoute to the package and is ignored for
	// HTTP endpoints (a proto package is not an HTTP path).
	Prefix string
}

// GRPC reports whether the endpoint speaks gRPC (vs an HTTP transport).
func (e ExposedEndpoint) GRPC() bool {
	return !standards.IsHTTPBasedAPI(e.API)
}

// Exposure is the backend-neutral description of one service's public surface.
type Exposure struct {
	// Service is the codefly service name, which is also the in-cluster
	// Service (backend) name.
	Service string
	// Namespace is the environment namespace the service is deployed into.
	Namespace string
	// Gateway is the shared gateway the routes attach to.
	Gateway GatewayRef
	// Endpoints are the public endpoints to route.
	Endpoints []ExposedEndpoint
	// EnableMTLS emits an Istio PeerAuthentication requiring STRICT mTLS to
	// the backend workload (selected by the conventional app=<service> label).
	EnableMTLS bool
}

// GatewayRef identifies the shared gateway routes attach to. A namespace
// different from the routes' namespace produces a cross-namespace parentRef.
type GatewayRef struct {
	Name      string
	Namespace string
}

// Renderer turns an Exposure into a single multi-document YAML manifest.
type Renderer interface {
	// Name is the stable identifier selected with --routing.
	Name() string
	Render(Exposure) (string, error)
}

var renderers = map[string]Renderer{
	gatewayAPIRenderer{}.Name(): gatewayAPIRenderer{},
	istioRenderer{}.Name():      istioRenderer{},
}

// Backends lists the registered renderer names, sorted.
func Backends() []string {
	names := make([]string, 0, len(renderers))
	for name := range renderers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Render selects a backend by name and renders the exposure.
func Render(backend string, exposure Exposure) (string, error) {
	renderer, ok := renderers[backend]
	if !ok {
		return "", fmt.Errorf("unknown routing backend %q (valid: %s)", backend, strings.Join(Backends(), ", "))
	}
	if err := exposure.validate(); err != nil {
		return "", err
	}
	return renderer.Render(exposure)
}

// dns1123Subdomain matches a valid Kubernetes object name (RFC 1123 subdomain).
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func (e Exposure) validate() error {
	if e.Service == "" {
		return fmt.Errorf("exposure requires a service name")
	}
	if e.Namespace == "" {
		return fmt.Errorf("exposure requires a namespace")
	}
	if e.Gateway.Name == "" {
		return fmt.Errorf("exposure requires a gateway name")
	}
	if len(e.Endpoints) == 0 {
		return fmt.Errorf("service %q has no public endpoints to expose", e.Service)
	}
	for _, endpoint := range e.Endpoints {
		name := e.Service + "-" + endpoint.Name
		if len(name) > 253 || !dns1123Subdomain.MatchString(name) {
			return fmt.Errorf("generated route name %q is not a valid Kubernetes name (service %q, endpoint %q)", name, e.Service, endpoint.Name)
		}
		// A route with no hosts and no path scope matches every hostname on
		// the shared gateway — a silent traffic hijack. A gRPC endpoint scoped
		// to a proto package is method-bound, so hostless is acceptable there;
		// anything else must name at least one host.
		if len(endpoint.Hosts) == 0 && !(endpoint.GRPC() && endpoint.Prefix != "") {
			hint := "declare ingress hosts or pass --host"
			if endpoint.GRPC() {
				hint = "declare ingress hosts, pass --host, or pass --prefix to scope by proto package"
			}
			return fmt.Errorf("endpoint %q of service %q has no ingress hosts and no path scope; refusing to emit a catch-all route (%s)", endpoint.Name, e.Service, hint)
		}
	}
	return nil
}

// gatewayList renders the "[namespace/]name" gateway reference Istio uses.
func (g GatewayRef) istioReference(routeNamespace string) string {
	if g.Namespace != "" && g.Namespace != routeNamespace {
		return g.Namespace + "/" + g.Name
	}
	return g.Name
}

func (e Exposure) backendHost() string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", e.Service, e.Namespace)
}

func joinDocuments(documents []string) string {
	return strings.Join(documents, "---\n")
}
