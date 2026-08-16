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
		mutate(&exposure)
		_, err := Render("gateway-api", exposure)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("mutation %q: err = %v", want, err)
		}
	}
}

func TestGatewayAPIGRPCRoute(t *testing.T) {
	exposure := Exposure{
		Service:   "accounts",
		Namespace: "acme",
		Hosts:     []string{"api.acme.dev"},
		Prefix:    "acme.accounts.v1",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{{Name: "grpc", API: "grpc", Port: 9090}},
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
                service: acme\.accounts\.v1\..*
          backendRefs:
            - name: accounts
              port: 9090
`
	assertRender(t, "gateway-api", exposure, want)
}

// A public HTTP endpoint with no prefix and no ingress hosts routes the whole
// gateway to the backend: no hostnames, no path match.
func TestGatewayAPIHTTPRouteNoPrefixNoHosts(t *testing.T) {
	exposure := Exposure{
		Service:   "web",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{{Name: "http", API: "http", Port: 8080}},
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
    rules:
        - backendRefs:
            - name: web
              port: 8080
`
	assertRender(t, "gateway-api", exposure, want)
}

// A gateway in another namespace produces a cross-namespace parentRef.
func TestGatewayAPICrossNamespaceParentRef(t *testing.T) {
	exposure := Exposure{
		Service:   "web",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw", Namespace: "gateway-system"},
		Endpoints: []ExposedEndpoint{{Name: "http", API: "http", Port: 8080}},
	}
	out, err := Render("gateway-api", exposure)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "namespace: gateway-system") {
		t.Fatalf("parentRef missing gateway namespace:\n%s", out)
	}
}

func TestIstioVirtualServiceWithMTLS(t *testing.T) {
	exposure := Exposure{
		Service:    "accounts",
		Namespace:  "acme",
		Hosts:      []string{"api.acme.dev"},
		Prefix:     "acme.accounts.v1",
		Gateway:    GatewayRef{Name: "cf-gw"},
		Endpoints:  []ExposedEndpoint{{Name: "grpc", API: "grpc", Port: 9090}},
		EnableMTLS: true,
	}
	const want = `apiVersion: networking.istio.io/v1beta1
kind: VirtualService
metadata:
    name: accounts
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

// With no ingress hosts the Istio backend falls back to a wildcard host, which
// the VirtualService schema requires.
func TestIstioVirtualServiceWildcardHost(t *testing.T) {
	exposure := Exposure{
		Service:   "web",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{{Name: "http", API: "http", Port: 8080}},
	}
	out, err := Render("istio", exposure)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hosts:\n        - '*'") {
		t.Fatalf("expected wildcard host:\n%s", out)
	}
}

func validExposure() Exposure {
	return Exposure{
		Service:   "accounts",
		Namespace: "acme",
		Gateway:   GatewayRef{Name: "cf-gw"},
		Endpoints: []ExposedEndpoint{{Name: "grpc", API: "grpc", Port: 9090}},
	}
}

func assertRender(t *testing.T, backend string, exposure Exposure, want string) {
	t.Helper()
	got, err := Render(backend, exposure)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != want {
		t.Fatalf("render mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
