package routing

import (
	"strings"
	"testing"
)

func TestBackendsAreSorted(t *testing.T) {
	got := Backends()
	want := []string{"gateway-api", "istio"}
	if len(got) != len(want) {
		t.Fatalf("backends = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backends = %v, want %v", got, want)
		}
	}
}

func TestRenderUnknownBackend(t *testing.T) {
	_, err := Render("nginx", validExposure())
	if err == nil || !strings.Contains(err.Error(), "unknown routing backend") {
		t.Fatalf("err = %v, want unknown backend", err)
	}
}

func TestRenderValidatesExposure(t *testing.T) {
	cases := map[string]func(*Exposure){
		"requires a service name": func(e *Exposure) { e.Service = "" },
		"requires a namespace":    func(e *Exposure) { e.Namespace = "" },
		"requires a gateway name": func(e *Exposure) { e.Gateway.Name = "" },
		"no public endpoints":     func(e *Exposure) { e.Endpoints = nil },
	}
	for want, mutate := range cases {
		exposure := validExposure()
		mutate(exposure)
		_, err := Render("gateway-api", exposure)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("mutation %q: err = %v", want, err)
		}
	}
}

// An HTTP endpoint with no hosts would produce a route that matches every
// hostname on the shared gateway; validation must refuse it rather than emit a
// silent traffic hijack.
func TestRenderRefusesCatchAllHTTPRoute(t *testing.T) {
	exposure := validExposure()
	exposure.Endpoints = []ExposedEndpoint{{Name: "http", API: "http", Port: 8080}}
	for _, backend := range Backends() {
		_, err := Render(backend, exposure)
		if err == nil || !strings.Contains(err.Error(), "catch-all") {
			t.Fatalf("%s: err = %v, want catch-all refusal", backend, err)
		}
	}
}

// A hostless gRPC endpoint is acceptable only when a proto-package prefix scopes
// it; without the prefix it is a catch-all and must be refused.
func TestRenderHostlessGRPCRequiresPrefix(t *testing.T) {
	exposure := validExposure()
	exposure.Endpoints = []ExposedEndpoint{{Name: "grpc", API: "grpc", Port: 9090}}
	if _, err := Render("gateway-api", exposure); err == nil || !strings.Contains(err.Error(), "catch-all") {
		t.Fatalf("hostless gRPC without prefix: err = %v", err)
	}
	exposure.Endpoints[0].Prefix = "acme.accounts.v1"
	if _, err := Render("gateway-api", exposure); err != nil {
		t.Fatalf("hostless gRPC with prefix should render: %v", err)
	}
}

func TestRenderRejectsInvalidObjectName(t *testing.T) {
	exposure := validExposure()
	exposure.Endpoints = []ExposedEndpoint{{Name: "Admin_API", API: "grpc", Port: 9090, Hosts: []string{"api.acme.dev"}}}
	_, err := Render("gateway-api", exposure)
	if err == nil || !strings.Contains(err.Error(), "not a valid Kubernetes name") {
		t.Fatalf("err = %v, want invalid-name refusal", err)
	}
}

func TestGatewayAPIGRPCRoute(t *testing.T) {
	exposure := &Exposure{
		Service:   "accounts",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{{Name: "grpc", API: "grpc", Port: 9090, Hosts: []string{"api.acme.dev"}, Prefix: "acme.accounts.v1"}},
	}
	const want = `apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
    name: accounts-grpc
    namespace: acme
    labels:
        app.kubernetes.io/managed-by: codefly
        codefly.dev/service: accounts
spec:
    parentRefs:
        - name: cf-gw
    hostnames:
        - api.acme.dev
    rules:
        - matches:
            - method:
                type: RegularExpression
                service: ^acme\.accounts\.v1\..*$
          backendRefs:
            - name: accounts
              port: 9090
`
	assertRender(t, "gateway-api", exposure, want)
}

// A public HTTP endpoint is host-scoped: it routes its hosts to the backend
// with no path match. The proto-package prefix is not applied to HTTP.
func TestGatewayAPIHTTPRouteIsHostScoped(t *testing.T) {
	exposure := &Exposure{
		Service:   "web",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{{Name: "http", API: "http", Port: 8080, Hosts: []string{"web.acme.dev"}, Prefix: "acme.accounts.v1"}},
	}
	const want = `apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
    name: web-http
    namespace: acme
    labels:
        app.kubernetes.io/managed-by: codefly
        codefly.dev/service: web
spec:
    parentRefs:
        - name: cf-gw
    hostnames:
        - web.acme.dev
    rules:
        - backendRefs:
            - name: web
              port: 8080
`
	assertRender(t, "gateway-api", exposure, want)
}

// A gateway in another namespace produces a cross-namespace parentRef.
func TestGatewayAPICrossNamespaceParentRef(t *testing.T) {
	exposure := &Exposure{
		Service:   "web",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw", Namespace: "gateway-system"},
		Endpoints: []ExposedEndpoint{{Name: "http", API: "http", Port: 8080, Hosts: []string{"web.acme.dev"}}},
	}
	out, err := Render("gateway-api", exposure)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "namespace: gateway-system") {
		t.Fatalf("parentRef missing gateway namespace:\n%s", out)
	}
}

// Each endpoint routes to its own port and object; a service with two distinct
// APIs must not collapse onto the first route.
func TestGatewayAPIMultipleEndpointsKeepDistinctPorts(t *testing.T) {
	exposure := &Exposure{
		Service:   "accounts",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{
			{Name: "grpc", API: "grpc", Port: 9090, Hosts: []string{"api.acme.dev"}, Prefix: "acme.accounts.v1"},
			{Name: "http", API: "http", Port: 7420, Hosts: []string{"api.acme.dev"}},
		},
	}
	out, err := Render("gateway-api", exposure)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "port: 9090") || !strings.Contains(out, "port: 7420") {
		t.Fatalf("expected both distinct ports:\n%s", out)
	}
	if strings.Count(out, "kind: GRPCRoute") != 1 || strings.Count(out, "kind: HTTPRoute") != 1 {
		t.Fatalf("expected one route object per endpoint:\n%s", out)
	}
}

func TestIstioVirtualServiceWithMTLS(t *testing.T) {
	exposure := &Exposure{
		Service:    "accounts",
		Namespace:  "acme",
		Gateway:    GatewayRef{Name: "cf-gw"},
		Endpoints:  []ExposedEndpoint{{Name: "grpc", API: "grpc", Port: 9090, Hosts: []string{"api.acme.dev"}, Prefix: "acme.accounts.v1"}},
		EnableMTLS: true,
	}
	const want = `apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
    name: accounts-grpc
    namespace: acme
    labels:
        app.kubernetes.io/managed-by: codefly
        codefly.dev/service: accounts
spec:
    hosts:
        - api.acme.dev
    gateways:
        - cf-gw
    http:
        - match:
            - uri:
                prefix: /acme.accounts.v1
          route:
            - destination:
                host: accounts.acme.svc.cluster.local
                port:
                    number: 9090
---
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
    name: accounts-mtls
    namespace: acme
    labels:
        app.kubernetes.io/managed-by: codefly
        codefly.dev/service: accounts
spec:
    selector:
        matchLabels:
            app: accounts
    mtls:
        mode: STRICT
`
	assertRender(t, "istio", exposure, want)
}

// The legacy Istio backend must emit one VirtualService per endpoint so that
// endpoints beyond the first stay routable (Istio stops at the first matching
// http rule within a VirtualService).
func TestIstioEmitsOneVirtualServicePerEndpoint(t *testing.T) {
	exposure := &Exposure{
		Service:   "accounts",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{
			{Name: "grpc", API: "grpc", Port: 9090, Hosts: []string{"api.acme.dev"}, Prefix: "acme.accounts.v1"},
			{Name: "rest", API: "rest", Port: 8080, Hosts: []string{"rest.acme.dev"}},
		},
	}
	out, err := Render("istio", exposure)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "kind: VirtualService") != 2 {
		t.Fatalf("expected one VirtualService per endpoint:\n%s", out)
	}
	if !strings.Contains(out, "number: 9090") || !strings.Contains(out, "number: 8080") {
		t.Fatalf("expected both endpoint ports routed:\n%s", out)
	}
}

func validExposure() *Exposure {
	return &Exposure{
		Service:   "accounts",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{{Name: "grpc", API: "grpc", Port: 9090, Hosts: []string{"api.acme.dev"}, Prefix: "acme.accounts.v1"}},
	}
}

func assertRender(t *testing.T, backend string, exposure *Exposure, want string) {
	t.Helper()
	got, err := Render(backend, exposure)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != want {
		t.Fatalf("render mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
