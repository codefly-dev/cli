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
	"sort"
	"strings"

	"github.com/codefly-dev/core/standards"
)

// ExposedEndpoint is one public endpoint to route to a backend.
type ExposedEndpoint struct {
	// Name is the endpoint name (used only to name generated objects).
	Name string
	// API is the codefly API kind (standards.GRPC/REST/HTTP/CONNECT).
	API string
	// Port is the in-cluster backend Service port. It is the canonical
	// per-API port the network layer binds (standards.Port), so callers do
	// not have to guess it.
	Port uint16
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
	// Hosts are the external hostnames the routes answer for, taken from the
	// environment ingress intent (or overridden explicitly). Empty means the
	// routes attach to every hostname the gateway listens on.
	Hosts []string
	// Prefix is the path-prefix contract for this surface: the proto package
	// for gRPC (matched as "<prefix>.*") and the URL path prefix for HTTP.
	// Empty means no prefix scoping (route the whole host to this backend).
	Prefix string
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
